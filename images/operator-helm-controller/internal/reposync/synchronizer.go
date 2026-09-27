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

package reposync

import (
	"context"

	"sigs.k8s.io/controller-runtime/pkg/client"

	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
	"github.com/deckhouse/operator-helm/internal/chartsource"
	repoclient "github.com/deckhouse/operator-helm/internal/client/repository"
	"github.com/deckhouse/operator-helm/internal/contract"
)

type Synchronizer struct {
	client        client.Client
	clientFactory RepoClientFactory
	catalog       contract.Catalog
}

// RepoClientFactory builds the client used to read a repository catalog. It is
// injected so the synchronization can be tested without a live repository.
type RepoClientFactory func(repoType chartsource.Kind) (repoclient.ClientInterface, error)

// NewSynchronizer builds the synchronization for one repository kind: the
// catalog decides which chart catalog kind the fetched charts are mirrored into.
func NewSynchronizer(client client.Client, factory RepoClientFactory, catalog contract.Catalog) *Synchronizer {
	if factory == nil {
		factory = repoclient.NewClient
	}

	return &Synchronizer{
		client:        client,
		clientFactory: factory,
		catalog:       catalog,
	}
}

// MigrateNames moves the repository's catalog objects to the names the current
// scheme derives. It is separate from Sync because it must run whether or not the
// remote can be reached: a consumer resolves a chart by the current name as soon as
// the new controller starts, while the first fetch may be minutes away or never
// succeed again.
//
// TRANSITIONAL: remove together with the catalog's own migration.
func (s *Synchronizer) MigrateNames(ctx context.Context, repo contract.Repository) error {
	return s.catalog.MigrateNames(ctx, repo)
}

// Sync reads the repository catalog and reconciles the chart catalog objects that
// mirror it. The two phases are reported separately: a fetch failure is about the
// remote, a catalog failure is about this cluster.
func (s *Synchronizer) Sync(
	ctx context.Context,
	repo contract.Repository,
	repoType chartsource.Kind,
) SyncOutcome {
	known, err := s.catalog.Known(ctx, repo)
	if err != nil {
		// The registry was never contacted: FetchAttempted stays false so the
		// caller does not mistake this cluster-side read failure for a fetch that
		// ran (let alone succeeded).
		return SyncOutcome{Catalog: CatalogOutcome{Err: err}}
	}

	forced := repo.ForceReconcileRequired()

	charts, fetch := s.fetchCharts(ctx, repo, repoType, repoclient.FetchOptions{
		Known: known,
		Full:  forced,
	})
	if fetch.Err != nil {
		return SyncOutcome{FetchAttempted: true, Fetch: fetch}
	}

	// The same flag the read was made under: the catalog records which versions a
	// forced read left without a fresh verdict, and reading the annotation a second
	// time could disagree with what was actually asked of the registry.
	result, err := s.catalog.Reconcile(ctx, repo, charts, forced)

	return SyncOutcome{
		FetchAttempted: true,
		Fetch:          fetch,
		Catalog:        CatalogOutcome{Err: err, SkippedCharts: result.SkippedCharts},
	}
}

func (s *Synchronizer) fetchCharts(
	ctx context.Context,
	repo contract.Repository,
	repoType chartsource.Kind,
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
		return charts, FetchOutcome{
			Pending:    countPending(charts),
			Unexamined: countUnexamined(charts),
			Charts:     len(charts),
		}
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

// countUnexamined counts the versions the client listed but never looked at. They are
// the measure of how far short of the repository this pass stopped, and the caller
// schedules the next one by it.
func countUnexamined(charts []repoclient.Chart) int {
	unexamined := 0
	for _, chart := range charts {
		unexamined += len(chart.UnexaminedVersions)
	}

	return unexamined
}

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

func buildRepoConfig(repo contract.Repository) *repoclient.RepoConfig {
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
