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

	"github.com/samber/lo"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/deckhouse/operator-helm/api/naming"
	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
	repoclient "github.com/deckhouse/operator-helm/internal/client/repository"
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

	charts, err := repoClient.FetchCharts(ctx, repo.Spec.URL, buildRepoConfig(repo), repoclient.FetchOptions{})
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

		addonChartName := naming.HelmClusterAddonChartName(repo.Name, chart.Name)
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
