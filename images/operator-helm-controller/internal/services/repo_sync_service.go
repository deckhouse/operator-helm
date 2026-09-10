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

	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
	repoclient "github.com/deckhouse/operator-helm/internal/client/repository"
	"github.com/deckhouse/operator-helm/internal/source"
	"github.com/deckhouse/operator-helm/internal/utils"
)

type RepoSyncService struct {
	BaseService

	clientFactory RepoClientFactory
	catalog       source.Catalog
}

// RepoClientFactory builds the client used to read a repository catalog. It is
// injected so the synchronization can be tested without a live repository.
type RepoClientFactory func(repoType utils.InternalRepositoryType) (repoclient.ClientInterface, error)

// NewRepoSyncService builds the synchronization for one repository kind: the
// catalog decides which chart catalog kind the fetched charts are mirrored into.
func NewRepoSyncService(client client.Client, scheme *runtime.Scheme, factory RepoClientFactory, catalog source.Catalog) *RepoSyncService {
	if factory == nil {
		factory = repoclient.NewClient
	}

	return &RepoSyncService{
		BaseService: BaseService{
			Client: client,
			Scheme: scheme,
		},
		clientFactory: factory,
		catalog:       catalog,
	}
}

// Sync reads the repository catalog and reconciles the chart catalog objects that
// mirror it. The two phases are reported separately: a fetch failure is about the
// remote, a catalog failure is about this cluster.
func (s *RepoSyncService) Sync(
	ctx context.Context,
	repo source.Repository,
	repoType utils.InternalRepositoryType,
) SyncOutcome {
	known, err := s.catalog.Known(ctx, repo)
	if err != nil {
		// The registry was never contacted: FetchAttempted stays false so the
		// caller does not mistake this cluster-side read failure for a fetch that
		// ran (let alone succeeded).
		return SyncOutcome{Catalog: CatalogOutcome{Err: err}}
	}

	charts, fetch := s.fetchCharts(ctx, repo, repoType, repoclient.FetchOptions{
		Known: known,
		Full:  repo.ForceReconcileRequired(),
	})
	if fetch.Err != nil {
		return SyncOutcome{FetchAttempted: true, Fetch: fetch}
	}

	return SyncOutcome{
		FetchAttempted: true,
		Fetch:          fetch,
		Catalog:        CatalogOutcome{Err: s.catalog.Reconcile(ctx, repo, charts)},
	}
}

func (s *RepoSyncService) fetchCharts(
	ctx context.Context,
	repo source.Repository,
	repoType utils.InternalRepositoryType,
	opts repoclient.FetchOptions,
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

	charts, err := repoClient.FetchCharts(ctx, repo.URL(), buildRepoConfig(repo), opts)
	if err == nil {
		return charts, FetchOutcome{Pending: countPending(charts)}
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

// countPending counts the versions that were listed but not examined in this pass.
func countPending(charts []repoclient.Chart) int {
	pending := 0
	for _, chart := range charts {
		for _, version := range chart.Versions {
			if version.UnavailableReason == helmv1alpha1.UnavailableReasonResolvePending {
				pending++
			}
		}
	}

	return pending
}

func buildRepoConfig(repo source.Repository) *repoclient.RepoConfig {
	if repo.Auth() == nil && repo.CACertificate() == "" && !repo.InsecureSkipVerify() {
		return nil
	}

	config := &repoclient.RepoConfig{
		Insecure:      repo.InsecureSkipVerify(),
		CACertificate: repo.CACertificate(),
	}

	if auth := repo.Auth(); auth != nil {
		config.Username = auth.Username
		config.Password = auth.Password
	}

	return config
}
