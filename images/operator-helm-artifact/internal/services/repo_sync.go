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
	"github.com/deckhouse/operator-helm/internal/common"
	"github.com/deckhouse/operator-helm/internal/utils"
)

const (
	ConditionTypeSynced = "Synced"

	// LabelRepositoryName stores HelmClusterAddonRepository name.
	LabelRepositoryName = "repository"

	// LabelChartName stores chart name.
	LabelChartName = "chart"

	ReasonSyncSucceeded      = "SyncSucceeded"
	ReasonSyncFailed         = "SyncFailed"
	ReasonRepositoryNotReady = "RepositoryNotReady"
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

type RepoSyncResult struct {
	Status ResourceStatus
}

func (r RepoSyncResult) GetStatus() ResourceStatus {
	return r.Status
}

func (r RepoSyncResult) IsReady() bool {
	return r.Status.IsReady()
}

func (r RepoSyncResult) GetConditionType() string {
	return ConditionTypeSynced
}

func (s *RepoSyncService) EnsureAddonCharts(ctx context.Context, repo *helmv1alpha1.HelmClusterAddonRepository, repoType utils.InternalRepositoryType) RepoSyncResult {
	logger := log.FromContext(ctx)

	if !isRepoSyncRequired(repo) {
		return RepoSyncResult{Status: Success(repo)}
	} else if !isRepoSyncInProgress(repo) {
		return RepoSyncResult{Status: Unknown(repo, ReasonReconciling)}
	}

	repoClient, err := repoclient.NewClient(repoType)
	if err != nil {
		return RepoSyncResult{
			Status: Failed(
				repo,
				ReasonSyncFailed,
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
			Status: Failed(
				repo,
				ReasonSyncFailed,
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
				common.LabelDeckhouseHeritage: common.LabelDeckhouseHeritageValue,
				LabelRepositoryName:           repo.Name,
				LabelChartName:                chart,
			}
			return nil
		})
		if err != nil {
			return RepoSyncResult{
				Status: Failed(
					repo,
					ReasonSyncFailed,
					fmt.Sprintf("Failed to create HelmClusterAddonChart %q", addonChartName),
					fmt.Errorf("cannot create or update HelmClusterAddonChart: %w", err),
				),
			}
		}

		existingVersionsMap := make(map[string]helmv1alpha1.HelmClusterAddonChartVersion)
		for _, version := range existing.Status.Versions {
			existingVersionsMap[version.Version] = version
		}

		for i, version := range versions {
			if existingVersion, found := existingVersionsMap[version.Version]; found && version.Digest == existingVersion.Digest {
				versions[i].Pulled = existingVersion.Pulled
			}
		}

		if op != controllerutil.OperationResultNone {
			logger.Info("Reconciled HelmClusterAddonChart", "operation", op, "addonChartName", addonChartName)
		}

		base := existing.DeepCopy()
		existing.Status.Versions = versions

		if err := s.Client.Status().Patch(ctx, existing, client.MergeFrom(base)); err != nil {
			return RepoSyncResult{
				Status: Failed(
					repo,
					ReasonSyncFailed,
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
			Status: Failed(
				repo,
				ReasonSyncFailed,
				"Failed to list stale charts for pruning",
				fmt.Errorf("listing existing charts for pruning: %w", err),
			),
		}
	}

	for i := range existingCharts.Items {
		staleChart := &existingCharts.Items[i]
		if _, wanted := desiredCharts[staleChart.Name]; wanted {
			continue
		}

		if err := s.ensureResourceDeleted(ctx, types.NamespacedName{Name: staleChart.Name}, staleChart); err != nil {
			return RepoSyncResult{
				Status: Failed(
					repo,
					ReasonSyncFailed,
					"Failed to delete stale charts",
					fmt.Errorf("deleting stale charts: %w", err),
				),
			}
		}
	}

	logger.Info(fmt.Sprintf("Scheduling next repo sync in %s", ChartsSyncInterval))

	return RepoSyncResult{
		Status: ResourceStatus{
			Status:             metav1.ConditionTrue,
			Reason:             ReasonSyncSucceeded,
			ObservedGeneration: repo.Generation,
			Message:            "",
			Err:                nil,
		},
	}
}

func isRepoSyncRequired(repo *helmv1alpha1.HelmClusterAddonRepository) bool {
	syncCond := apimeta.FindStatusCondition(repo.Status.Conditions, ConditionTypeSynced)
	if syncCond != nil && syncCond.Status == metav1.ConditionTrue && syncCond.LastTransitionTime.UTC().Add(ChartsSyncInterval).After(time.Now().UTC()) {
		return false
	}
	return true
}

func isRepoSyncInProgress(repo *helmv1alpha1.HelmClusterAddonRepository) bool {
	syncCond := apimeta.FindStatusCondition(repo.Status.Conditions, ConditionTypeSynced)
	if syncCond != nil && syncCond.Status == metav1.ConditionUnknown && syncCond.Reason == ReasonReconciling {
		return true
	}

	return false
}
