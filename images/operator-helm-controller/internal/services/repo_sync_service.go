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

	"github.com/samber/lo"
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

	clientFactory RepoClientFactory
}

// RepoClientFactory builds the client used to read a repository catalog. It is
// injected so the synchronization can be tested without a live repository.
type RepoClientFactory func(repoType utils.InternalRepositoryType) (repoclient.ClientInterface, error)

func NewRepoSyncService(client client.Client, scheme *runtime.Scheme, factory RepoClientFactory) *RepoSyncService {
	if factory == nil {
		factory = repoclient.NewClient
	}

	return &RepoSyncService{
		BaseService: BaseService{
			Client: client,
			Scheme: scheme,
		},
		clientFactory: factory,
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

// InProgress reports the first phase of the sync state machine: the Synced
// condition has just been marked Reconciling and the actual chart fetch still
// needs to run. The caller performs that fetch in the same reconcile so progress
// does not depend on a status-update watch event.
func (r RepoSyncResult) InProgress() bool {
	return r.Status.Status == metav1.ConditionUnknown && r.Status.Reason == helmv1alpha1.ReasonReconciling
}

func (s *RepoSyncService) EnsureAddonCharts(ctx context.Context, repo *helmv1alpha1.HelmClusterAddonRepository, repoType utils.InternalRepositoryType) RepoSyncResult {
	if !isRepoSyncRequired(repo) {
		return RepoSyncResult{Status: status.Empty()}
	} else if !isRepoSyncInProgress(repo) {
		return RepoSyncResult{Status: status.Unknown(repo, helmv1alpha1.ReasonReconciling)}
	}

	outcome := s.Sync(ctx, repo, repoType)

	switch {
	case outcome.Fetch.Err != nil:
		return RepoSyncResult{Status: status.Failed(repo, helmv1alpha1.ReasonSyncFailed, outcome.Fetch.Message, outcome.Fetch.Err)}
	case outcome.Catalog.Err != nil:
		return RepoSyncResult{Status: status.Failed(repo, helmv1alpha1.ReasonSyncFailed, "Failed to update the chart catalog", outcome.Catalog.Err)}
	default:
		return RepoSyncResult{Status: status.Success(repo)}
	}
}

// Sync reads the repository catalog and reconciles the HelmClusterAddonChart
// resources that mirror it. The two phases are reported separately: a fetch
// failure is about the remote, a catalog failure is about this cluster.
func (s *RepoSyncService) Sync(
	ctx context.Context,
	repo *helmv1alpha1.HelmClusterAddonRepository,
	repoType utils.InternalRepositoryType,
) SyncOutcome {
	charts, fetch := s.fetchCharts(ctx, repo, repoType)
	if fetch.Err != nil {
		return SyncOutcome{Fetch: fetch}
	}

	return SyncOutcome{Fetch: fetch, Catalog: s.reconcileCatalog(ctx, repo, charts)}
}

func (s *RepoSyncService) fetchCharts(
	ctx context.Context,
	repo *helmv1alpha1.HelmClusterAddonRepository,
	repoType utils.InternalRepositoryType,
) ([]repoclient.Chart, FetchOutcome) {
	repoClient, err := s.clientFactory(repoType)
	if err != nil {
		return nil, FetchOutcome{
			Err:      err,
			Terminal: true,
			Reason:   helmv1alpha1.ReasonUnsupportedRepositoryType,
			Message:  "Unsupported repository type",
		}
	}

	charts, err := repoClient.FetchCharts(ctx, repo.Spec.URL, buildRepoConfig(repo))
	if err == nil {
		return charts, FetchOutcome{}
	}

	if terminal, ok := repoclient.AsTerminal(err); ok {
		return nil, FetchOutcome{
			Err:      err,
			Terminal: true,
			Reason:   terminal.Reason,
			Message:  terminal.Message,
		}
	}

	return nil, FetchOutcome{
		Err:     err,
		Reason:  helmv1alpha1.ReasonSyncFailed,
		Message: "Failed to read the repository catalog: " + err.Error(),
	}
}

func buildRepoConfig(repo *helmv1alpha1.HelmClusterAddonRepository) *repoclient.RepoConfig {
	if repo.Spec.Auth == nil && repo.Spec.CACertificate == "" && !repo.Spec.InsecureSkipVerify {
		return nil
	}

	config := &repoclient.RepoConfig{
		Insecure:      repo.Spec.InsecureSkipVerify,
		CACertificate: repo.Spec.CACertificate,
	}

	if repo.Spec.Auth != nil {
		config.Username = repo.Spec.Auth.Username
		config.Password = repo.Spec.Auth.Password
	}

	return config
}

func (s *RepoSyncService) reconcileCatalog(
	ctx context.Context,
	repo *helmv1alpha1.HelmClusterAddonRepository,
	charts []repoclient.Chart,
) CatalogOutcome {
	logger := log.FromContext(ctx)

	desiredCharts := make(map[string]struct{}, len(charts))

	for _, chart := range charts {
		if len(chart.Versions) == 0 {
			continue
		}

		addonChartName := utils.GetHelmClusterAddonChartName(repo.Name, chart.Name)
		existing := &helmv1alpha1.HelmClusterAddonChart{
			ObjectMeta: metav1.ObjectMeta{Name: addonChartName},
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
				LabelChartName:                      chart.Name,
			}

			return nil
		})
		if err != nil {
			return CatalogOutcome{Err: fmt.Errorf("creating or updating chart %q: %w", addonChartName, err)}
		}

		if op != controllerutil.OperationResultNone {
			logger.Info("Reconciled HelmClusterAddonChart", "operation", op, "addonChartName", addonChartName)
		}

		base := existing.DeepCopy()

		existing.Status.IconURL = chart.Versions[0].IconURL
		existing.Status.Versions = lo.Map(chart.Versions, func(v repoclient.ChartVersion, _ int) helmv1alpha1.HelmClusterAddonChartVersion {
			return helmv1alpha1.HelmClusterAddonChartVersion{Version: v.Version.Original()}
		})

		if err := s.Client.Status().Patch(ctx, existing, client.MergeFrom(base)); err != nil {
			return CatalogOutcome{Err: fmt.Errorf("updating versions of chart %q: %w", addonChartName, err)}
		}
	}

	var existingCharts helmv1alpha1.HelmClusterAddonChartList
	if err := s.Client.List(ctx, &existingCharts, client.MatchingLabels{LabelRepositoryName: repo.Name}); err != nil {
		return CatalogOutcome{Err: fmt.Errorf("listing charts for pruning: %w", err)}
	}

	for _, chart := range existingCharts.Items {
		if _, wanted := desiredCharts[chart.Name]; wanted {
			continue
		}

		if err := s.ensureResourceDeleted(ctx, types.NamespacedName{Name: chart.Name}, &chart); err != nil {
			return CatalogOutcome{Err: fmt.Errorf("deleting stale charts: %w", err)}
		}
	}

	return CatalogOutcome{}
}

func isRepoSyncRequired(repo *helmv1alpha1.HelmClusterAddonRepository) bool {
	if repo.ForceReconcileRequired() {
		return true
	}

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
