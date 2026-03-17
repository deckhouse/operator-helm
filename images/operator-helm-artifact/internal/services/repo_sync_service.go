/*
Copyright 2026 Flant JSC.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package services

import (
	"context"
	"fmt"
	"time"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
	repoclient "github.com/deckhouse/operator-helm/internal/client/repository"
	"github.com/deckhouse/operator-helm/internal/manager/status"
	"github.com/deckhouse/operator-helm/internal/utils"
)

const (

	// LabelRepositoryName stores HelmClusterAddonRepository name.
	LabelRepositoryName = "repository"

	// LabelChartName stores chart name.
	LabelChartName = "chart"
)

type RepoSyncService struct {
	BaseService
}

func NewRepoSyncService(client client.Client, scheme *runtime.Scheme) *RepoSyncService {
	return &RepoSyncService{
		BaseService: BaseService{
			Client: client,
			Scheme: scheme,
		},
	}
}

var _ status.Provider = (*RepoSyncResult)(nil)

type RepoSyncResult struct {
	Status status.Status
}

func (r RepoSyncResult) GetStatus() status.Status {
	return r.Status
}

func (r RepoSyncResult) IsReady() bool {
	return r.Status.IsReady()
}

func (r RepoSyncResult) GetConditionType() string {
	return helmv1alpha1.ConditionTypeSynced
}

func (s *RepoSyncService) EnsureAddonCharts(ctx context.Context, repo *helmv1alpha1.HelmClusterAddonRepository, repoType utils.InternalRepositoryType) RepoSyncResult {
	logger := log.FromContext(ctx)

	if !isRepoSyncRequired(repo) {
		return RepoSyncResult{Status: status.Empty()}
	} else if !isRepoSyncInProgress(repo) {
		return RepoSyncResult{Status: status.Unknown(repo, helmv1alpha1.ReasonReconciling)}
	}

	repoClient, err := repoclient.NewClient(repoType)
	if err != nil {
		return RepoSyncResult{
			Status: status.Failed(
				repo,
				helmv1alpha1.ReasonSyncFailed,
				"Failed to get repository client on chart sync",
				fmt.Errorf("getting repository client: %w", err),
			),
		}
	}

	var authConfig *repoclient.AuthConfig
	if repo.Spec.Auth != nil {
		authConfig = &repoclient.AuthConfig{
			Username: repo.Spec.Auth.Username,
			Password: repo.Spec.Auth.Password,
		}
	}

	charts, err := repoClient.FetchCharts(ctx, repo.Spec.URL, authConfig)
	if err != nil {
		return RepoSyncResult{
			Status: status.Failed(
				repo,
				helmv1alpha1.ReasonSyncFailed,
				"Failed to fetch charts from repository",
				fmt.Errorf("fetching charts: %w", err),
			),
		}
	}

	desiredCharts := make(map[string]struct{}, len(charts))

	for chart, versions := range charts {
		addonChartName := utils.GetHelmClusterAddonChartName(repo.Name, chart)
		existing := &helmv1alpha1.HelmClusterAddonChart{
			ObjectMeta: metav1.ObjectMeta{
				Name: addonChartName,
			},
		}

		desiredCharts[existing.Name] = struct{}{}

		op, err := controllerutil.CreateOrPatch(ctx, s.Client, existing, func() error {
			existing.OwnerReferences = []metav1.OwnerReference{
				{
					APIVersion:         repo.APIVersion,
					Kind:               repo.Kind,
					Name:               repo.Name,
					UID:                repo.UID,
					Controller:         ptr.To(true),
					BlockOwnerDeletion: ptr.To(true),
				},
			}
			existing.Labels = map[string]string{
				helmv1alpha1.LabelDeckhouseHeritage: helmv1alpha1.LabelDeckhouseHeritageValue,
				LabelRepositoryName:                 repo.Name,
				LabelChartName:                      chart,
			}
			return nil
		})
		if err != nil {
			return RepoSyncResult{
				Status: status.Failed(
					repo,
					helmv1alpha1.ReasonSyncFailed,
					fmt.Sprintf("Failed to create HelmClusterAddonChart %q", addonChartName),
					fmt.Errorf("cannot create or update HelmClusterAddonChart: %w", err),
				),
			}
		}

		if op != controllerutil.OperationResultNone {
			logger.Info("Reconciled HelmClusterAddonChart", "operation", op, "addonChartName", addonChartName)
		}

		base := existing.DeepCopy()
		existing.Status.Versions = versions

		if err := s.Client.Status().Patch(ctx, existing, client.MergeFrom(base)); err != nil {
			return RepoSyncResult{
				Status: status.Failed(
					repo,
					helmv1alpha1.ReasonSyncFailed,
					fmt.Sprintf("Failed to update HelmClusterAddonChart %q versions", addonChartName),
					fmt.Errorf("updating chart versions: %w", err),
				),
			}
		}

		logger.Info("Successfully synced HelmClusterAddonChart versions", "operation", op, "addonChartName", addonChartName)
	}

	var existingCharts helmv1alpha1.HelmClusterAddonChartList
	if err := s.Client.List(ctx, &existingCharts, client.MatchingLabels{LabelRepositoryName: repo.Name}); err != nil {
		return RepoSyncResult{
			Status: status.Failed(
				repo,
				helmv1alpha1.ReasonSyncFailed,
				"Failed to list stale charts for pruning",
				fmt.Errorf("listing existing charts for pruning: %w", err),
			),
		}
	}

	for _, chart := range existingCharts.Items {
		if _, wanted := desiredCharts[chart.Name]; wanted {
			continue
		}

		if err := s.ensureResourceDeleted(ctx, types.NamespacedName{Name: chart.Name}, &chart); err != nil {
			return RepoSyncResult{
				Status: status.Failed(
					repo,
					helmv1alpha1.ReasonSyncFailed,
					"Failed to delete stale charts",
					fmt.Errorf("deleting stale charts: %w", err),
				),
			}
		}
	}

	logger.Info(fmt.Sprintf("Scheduling next repo sync in %s", ChartsSyncInterval))

	return RepoSyncResult{
		Status: status.Success(repo),
	}
}

func isRepoSyncRequired(repo *helmv1alpha1.HelmClusterAddonRepository) bool {
	syncCond := apimeta.FindStatusCondition(repo.Status.Conditions, helmv1alpha1.ConditionTypeSynced)
	if syncCond != nil && syncCond.Status == metav1.ConditionTrue && syncCond.LastTransitionTime.UTC().Add(ChartsSyncInterval).After(time.Now().UTC()) {
		return false
	}
	return true
}

func isRepoSyncInProgress(repo *helmv1alpha1.HelmClusterAddonRepository) bool {
	syncCond := apimeta.FindStatusCondition(repo.Status.Conditions, helmv1alpha1.ConditionTypeSynced)
	if syncCond != nil && syncCond.Status == metav1.ConditionUnknown && syncCond.Reason == helmv1alpha1.ReasonReconciling {
		return true
	}

	return false
}
