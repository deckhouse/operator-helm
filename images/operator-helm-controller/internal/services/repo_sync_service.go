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
	"sort"

	"github.com/Masterminds/semver/v3"
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
	"github.com/deckhouse/operator-helm/internal/index"
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
	known, err := s.knownCharts(ctx, repo)
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

	return SyncOutcome{FetchAttempted: true, Fetch: fetch, Catalog: s.reconcileCatalog(ctx, repo, charts)}
}

// knownCharts collects the verdicts recorded by previous passes, so the client can
// skip the tags it has already examined. The chart objects are the only store of that
// state: keeping a separate fingerprint would be one more thing to drift.
func (s *RepoSyncService) knownCharts(
	ctx context.Context,
	repo *helmv1alpha1.HelmClusterAddonRepository,
) (repoclient.KnownCharts, error) {
	var charts helmv1alpha1.HelmClusterAddonChartList
	if err := s.Client.List(ctx, &charts, client.MatchingLabels{LabelRepositoryName: repo.Name}); err != nil {
		return nil, fmt.Errorf("listing charts of repository %q: %w", repo.Name, err)
	}

	logger := log.FromContext(ctx)
	known := make(repoclient.KnownCharts, len(charts.Items))

	for _, chart := range charts.Items {
		chartName := chart.Labels[LabelChartName]
		if chartName == "" {
			// The chart label is the only way back from the object name (a
			// truncated hash) to the chart name it belongs to. Without it the
			// recorded verdicts for this chart cannot be looked up here, so every
			// tag is re-examined on the next fetch; that is safe but not free, so
			// it is worth surfacing.
			logger.Info("Chart object has no chart label, dropping its recorded verdicts", "addonChartName", chart.Name)

			continue
		}

		versions := make(repoclient.KnownVersions, len(chart.Status.Versions))
		for _, version := range chart.Status.Versions {
			versions[version.Version] = repoclient.KnownVersion{
				MediaType:          version.MediaType,
				UnavailableReason:  version.UnavailableReason,
				UnavailableMessage: version.UnavailableMessage,
			}
		}

		known[chartName] = versions
	}

	return known, nil
}

func (s *RepoSyncService) fetchCharts(
	ctx context.Context,
	repo *helmv1alpha1.HelmClusterAddonRepository,
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

	charts, err := repoClient.FetchCharts(ctx, repo.Spec.URL, buildRepoConfig(repo), opts)
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
		addonChartName := naming.HelmClusterAddonChartName(repo.Name, chart.Name)
		// A chart with no usable version is still created: it carries the reason each of
		// its versions is unusable, and skipping it here would let the pruning loop below
		// delete a chart whose tags merely failed to resolve.
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

		inUse, err := s.inUseVersions(ctx, repo.Name, chart.Name)
		if err != nil {
			return CatalogOutcome{Err: err}
		}

		base := existing.DeepCopy()

		if len(chart.Versions) > 0 {
			existing.Status.IconURL = chart.Versions[0].IconURL
		}
		existing.Status.Versions = mergeChartVersions(chart.Versions, existing.Status.Versions, inUse)

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

		chartName := chart.Labels[LabelChartName]
		if chartName == "" {
			// The chart label is the only way back from the object name (a
			// truncated hash) to the chart name an addon references, so
			// inUseVersions cannot find anything to protect and this chart is
			// pruned even if an addon still uses it. That fail-open is unavoidable
			// as written, so at least make it diagnosable.
			logger.Info("Pruning a chart with no chart label; in-use protection could not be checked", "addonChartName", chart.Name)
		}

		inUse, err := s.inUseVersions(ctx, repo.Name, chartName)
		if err != nil {
			return CatalogOutcome{Err: err}
		}
		if len(inUse) > 0 {
			// An addon still references this chart: deleting the object would make the
			// addon's own reconciliation fail on a missing chart and block every change
			// to it, including its removal.
			logger.Info("Keeping a chart referenced by an addon", "addonChartName", chart.Name)

			continue
		}

		if err := s.ensureResourceDeleted(ctx, types.NamespacedName{Name: chart.Name}, &chart); err != nil {
			return CatalogOutcome{Err: fmt.Errorf("deleting stale charts: %w", err)}
		}
	}

	return CatalogOutcome{}
}

// inUseVersions returns the chart versions referenced by the addon that uses this
// repository/chart pair. The webhook and the claim Lease enforce one addon per pair,
// so at most one is found; both its desired and its last applied version count, since
// they differ during an upgrade.
func (s *RepoSyncService) inUseVersions(ctx context.Context, repoName, chartName string) (map[string]struct{}, error) {
	if chartName == "" {
		return nil, nil
	}

	var addons helmv1alpha1.HelmClusterAddonList
	if err := s.Client.List(ctx, &addons, client.MatchingFields{
		index.AddonChart: index.AddonChartValue(repoName, chartName),
	}); err != nil {
		return nil, fmt.Errorf("listing addons of chart %q: %w", chartName, err)
	}

	inUse := make(map[string]struct{}, 2)

	for _, addon := range addons.Items {
		inUse[addon.Spec.Chart.Version] = struct{}{}

		// LastAppliedChart carries its own repository/chart identity and can lag
		// behind Spec.Chart when an addon is switched to a different chart: only
		// credit it here when it still names this repository/chart pair, or a
		// stale entry would protect a phantom version on the new chart while no
		// longer protecting the version actually applied on the old one.
		if last := addon.Status.LastAppliedChart; last != nil &&
			last.HelmClusterAddonChartName == chartName && last.HelmClusterAddonRepository == repoName {
			inUse[last.Version] = struct{}{}
		}
	}

	return inUse, nil
}

// mergeChartVersions builds the desired version list from the fetched entries and the
// ones already recorded. A recorded version the registry no longer lists is dropped,
// unless an addon still references it: then it is retained with RemovedFromRepository
// and keeps its media type, without which the addon's internal OCIRepository could not
// be built at all.
func mergeChartVersions(
	fetched []repoclient.ChartVersion,
	current []helmv1alpha1.HelmClusterAddonChartVersion,
	inUse map[string]struct{},
) []helmv1alpha1.HelmClusterAddonChartVersion {
	merged := make([]helmv1alpha1.HelmClusterAddonChartVersion, 0, len(fetched)+len(current))
	listed := make(map[string]struct{}, len(fetched))

	for _, version := range fetched {
		name := version.Version.Original()
		listed[name] = struct{}{}

		merged = append(merged, helmv1alpha1.HelmClusterAddonChartVersion{
			Version:            name,
			MediaType:          version.MediaType,
			UnavailableReason:  version.UnavailableReason,
			UnavailableMessage: version.UnavailableMessage,
		})
	}

	for _, version := range current {
		if _, stillListed := listed[version.Version]; stillListed {
			continue
		}
		if _, referenced := inUse[version.Version]; !referenced {
			continue
		}

		version.UnavailableReason = helmv1alpha1.UnavailableReasonRemovedFromRepository
		version.UnavailableMessage = "the repository no longer offers this version"
		merged = append(merged, version)
	}

	sortChartVersions(merged)

	return merged
}

// sortChartVersions orders versions by descending semver, breaking ties by a reverse
// string comparison. A version that does not parse as semver sorts after every
// version that does, ordered among themselves by the same reverse string comparison.
// Parsability has to be the primary key: comparing a parsable and an unparsable
// version by semver on one pair and by string on another can produce a cycle (e.g.
// "6.10.0" > "6.9.0" by semver, "6.9.0" > "6.5.x" and "6.5.x" > "6.10.0" by string),
// which is not a valid ordering for sort.SliceStable. Today's clients never write an
// unparsable version, but legacy status data can still carry one, and the order has to
// be deterministic regardless: the merge goes through maps, and an unstable order
// would produce a status patch on every synchronization for a catalog that did not
// change.
func sortChartVersions(versions []helmv1alpha1.HelmClusterAddonChartVersion) {
	sort.SliceStable(versions, func(i, j int) bool {
		left, leftErr := semver.NewVersion(versions[i].Version)
		right, rightErr := semver.NewVersion(versions[j].Version)

		leftParses, rightParses := leftErr == nil, rightErr == nil

		if leftParses != rightParses {
			return leftParses
		}

		if leftParses && !left.Equal(right) {
			return left.GreaterThan(right)
		}

		return versions[i].Version > versions[j].Version
	})
}
