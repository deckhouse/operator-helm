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
	"errors"
	"testing"

	"github.com/Masterminds/semver/v3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/deckhouse/operator-helm/api/naming"
	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
	"github.com/deckhouse/operator-helm/internal/adapter"
	"github.com/deckhouse/operator-helm/internal/chartsource"
	repoclient "github.com/deckhouse/operator-helm/internal/client/repository"
	"github.com/deckhouse/operator-helm/internal/index"
)

type stubRepoClient struct {
	charts []repoclient.Chart
	err    error
}

func (s stubRepoClient) FetchCharts(_ context.Context, _ string, _ *repoclient.RepoConfig, _ repoclient.FetchOptions) ([]repoclient.Chart, error) {
	return s.charts, s.err
}

type recordingRepoClient struct {
	charts []repoclient.Chart
	opts   repoclient.FetchOptions
}

func (s *recordingRepoClient) FetchCharts(_ context.Context, _ string, _ *repoclient.RepoConfig, opts repoclient.FetchOptions) ([]repoclient.Chart, error) {
	s.opts = opts

	return s.charts, nil
}

func newSynchronizer(t *testing.T, stub stubRepoClient, objects ...client.Object) (*Synchronizer, client.Client) {
	t.Helper()

	scheme := testScheme(t)
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objects...).
		WithStatusSubresource(&helmv1alpha1.HelmClusterAddonChart{}, &helmv1alpha1.HelmClusterAddonRepository{}).
		WithIndex(&helmv1alpha1.HelmClusterAddon{}, index.AddonChart, func(obj client.Object) []string {
			addon := obj.(*helmv1alpha1.HelmClusterAddon)

			return []string{index.AddonChartValue(
				addon.Spec.Chart.HelmClusterAddonRepository,
				addon.Spec.Chart.HelmClusterAddonChartName,
			)}
		}).
		Build()

	factory := func(_ chartsource.Kind) (repoclient.ClientInterface, error) {
		return stub, nil
	}

	return NewSynchronizer(c, factory, adapter.NewAddonCatalog(c)), c
}

func ociVersion(version, mediaType string) repoclient.ChartVersion { //nolint:unparam // the parameter names the value the assertions read; inlining it would hide what the fixture stands for
	return repoclient.ChartVersion{Version: semver.MustParse(version), MediaType: mediaType}
}

func existingChart(repoName, chartName string, versions ...helmv1alpha1.ChartVersion) *helmv1alpha1.HelmClusterAddonChart { //nolint:unparam // the parameter names the value the assertions read; inlining it would hide what the fixture stands for
	return &helmv1alpha1.HelmClusterAddonChart{
		ObjectMeta: metav1.ObjectMeta{
			Name:   naming.HelmClusterAddonChartName(repoName, chartName),
			Labels: map[string]string{helmv1alpha1.LabelRepositoryName: repoName, helmv1alpha1.LabelChartName: chartName},
		},
		Status: helmv1alpha1.ChartCatalogStatus{Versions: versions},
	}
}

func addonUsing(repoName, chartName, version string) *helmv1alpha1.HelmClusterAddon {
	return &helmv1alpha1.HelmClusterAddon{
		ObjectMeta: metav1.ObjectMeta{Name: "consumer"},
		Spec: helmv1alpha1.HelmClusterAddonSpec{
			Namespace: "app",
			Chart: helmv1alpha1.HelmClusterAddonChartRef{
				HelmClusterAddonRepository: repoName,
				HelmClusterAddonChartName:  chartName,
				Version:                    version,
			},
		},
	}
}

func chartStatus(t *testing.T, c client.Client, repoName, chartName string) helmv1alpha1.ChartCatalogStatus { //nolint:unparam // the parameter names the value the assertions read; inlining it would hide what the fixture stands for
	t.Helper()

	chart := &helmv1alpha1.HelmClusterAddonChart{}
	key := client.ObjectKey{Name: naming.HelmClusterAddonChartName(repoName, chartName)}
	if err := c.Get(context.Background(), key, chart); err != nil {
		t.Fatalf("getting chart: %v", err)
	}

	return chart.Status
}

func chartFixture(name, version string) repoclient.Chart {
	return repoclient.Chart{
		Name:     name,
		Versions: []repoclient.ChartVersion{{Version: semver.MustParse(version), IconURL: "https://example.invalid/icon.png"}},
	}
}

func TestSynchronizerSyncCreatesChartsAndRecordsVersions(t *testing.T) {
	repo := testRepository()
	service, c := newSynchronizer(t, stubRepoClient{charts: []repoclient.Chart{chartFixture("podinfo", "6.7.1")}}, repo)

	outcome := service.Sync(context.Background(), adapter.NewAddonRepository(repo), chartsource.Helm)
	if outcome.Fetch.Err != nil {
		t.Fatalf("fetch failed: %v", outcome.Fetch.Err)
	}
	if outcome.Catalog.Err != nil {
		t.Fatalf("catalog update failed: %v", outcome.Catalog.Err)
	}

	chart := &helmv1alpha1.HelmClusterAddonChart{}
	key := client.ObjectKey{Name: naming.HelmClusterAddonChartName(repo.Name, "podinfo")}
	if err := c.Get(context.Background(), key, chart); err != nil {
		t.Fatalf("chart was not created: %v", err)
	}
	if len(chart.Status.Versions) != 1 || chart.Status.Versions[0].Version != "6.7.1" {
		t.Fatalf("chart versions are %v, want [6.7.1]", chart.Status.Versions)
	}
}

func TestSynchronizerSyncPrunesStaleCharts(t *testing.T) {
	repo := testRepository()
	stale := &helmv1alpha1.HelmClusterAddonChart{
		ObjectMeta: metav1.ObjectMeta{
			Name:   naming.HelmClusterAddonChartName(repo.Name, "removed"),
			Labels: map[string]string{helmv1alpha1.LabelRepositoryName: repo.Name, helmv1alpha1.LabelChartName: "removed"},
		},
	}

	service, c := newSynchronizer(t, stubRepoClient{charts: []repoclient.Chart{chartFixture("podinfo", "6.7.1")}}, repo, stale)

	outcome := service.Sync(context.Background(), adapter.NewAddonRepository(repo), chartsource.Helm)
	if outcome.Catalog.Err != nil {
		t.Fatalf("catalog update failed: %v", outcome.Catalog.Err)
	}

	err := c.Get(context.Background(), client.ObjectKeyFromObject(stale), &helmv1alpha1.HelmClusterAddonChart{})
	if err == nil {
		t.Fatal("stale chart must be pruned")
	}
}

func TestSynchronizerSyncReportsTerminalFetchFailure(t *testing.T) {
	repo := testRepository()
	terminal := &repoclient.TerminalError{
		Reason:  helmv1alpha1.ReasonAuthenticationFailed,
		Message: "repository rejected the credentials (HTTP 401)",
	}

	service, _ := newSynchronizer(t, stubRepoClient{err: terminal}, repo)

	outcome := service.Sync(context.Background(), adapter.NewAddonRepository(repo), chartsource.Helm)
	if outcome.Fetch.Err == nil {
		t.Fatal("expected a fetch failure")
	}
	if !outcome.Fetch.Terminal {
		t.Fatal("a TerminalError must be reported as terminal")
	}
	if outcome.Fetch.Reason != helmv1alpha1.ReasonAuthenticationFailed {
		t.Fatalf("fetch reason is %q, want %q", outcome.Fetch.Reason, helmv1alpha1.ReasonAuthenticationFailed)
	}
}

func TestSynchronizerSyncReportsTransientFetchFailure(t *testing.T) {
	repo := testRepository()
	service, _ := newSynchronizer(t, stubRepoClient{err: errors.New("connection refused")}, repo)

	outcome := service.Sync(context.Background(), adapter.NewAddonRepository(repo), chartsource.Helm)
	if outcome.Fetch.Err == nil {
		t.Fatal("expected a fetch failure")
	}
	if outcome.Fetch.Terminal {
		t.Fatal("a plain error must stay retriable")
	}
	if outcome.Fetch.Reason != helmv1alpha1.ReasonSyncFailed {
		t.Fatalf("fetch reason is %q, want %q", outcome.Fetch.Reason, helmv1alpha1.ReasonSyncFailed)
	}
}

func TestSynchronizerSyncPassesKnownVersionsToTheClient(t *testing.T) {
	repo := testRepository()
	chart := existingChart(repo.Name, "podinfo",
		helmv1alpha1.ChartVersion{Version: "6.7.1", MediaType: "application/tar+gzip"},
		helmv1alpha1.ChartVersion{
			Version:            "6.7.2",
			UnavailableReason:  helmv1alpha1.UnavailableReasonUnsupportedMediaType,
			UnavailableMessage: "layer media type application/vnd.example is not a chart",
		},
	)

	stub := &recordingRepoClient{charts: []repoclient.Chart{{
		Name:     "podinfo",
		Versions: []repoclient.ChartVersion{ociVersion("6.7.1", "application/tar+gzip")},
	}}}

	service, _ := newSynchronizer(t, stubRepoClient{}, repo, chart)
	service.clientFactory = func(_ chartsource.Kind) (repoclient.ClientInterface, error) {
		return stub, nil
	}

	if outcome := service.Sync(context.Background(), adapter.NewAddonRepository(repo), chartsource.OCI); outcome.Fetch.Err != nil {
		t.Fatalf("fetch failed: %v", outcome.Fetch.Err)
	}

	known := stub.opts.Known["podinfo"]
	if known["6.7.1"].MediaType != "application/tar+gzip" {
		t.Fatalf("known media type is %q", known["6.7.1"].MediaType)
	}
	if known["6.7.2"].UnavailableReason != helmv1alpha1.UnavailableReasonUnsupportedMediaType {
		t.Fatalf("known reason is %q", known["6.7.2"].UnavailableReason)
	}
	if known["6.7.2"].UnavailableMessage != "layer media type application/vnd.example is not a chart" {
		t.Fatalf("known message is %q", known["6.7.2"].UnavailableMessage)
	}
	if stub.opts.Full {
		t.Fatal("a normal pass must not request a full re-index")
	}
}

func TestSynchronizerSyncRequestsFullPassOnForceReconcile(t *testing.T) {
	repo := testRepository()
	repo.Annotations = map[string]string{helmv1alpha1.AnnotationForceReconcile: ""}

	stub := &recordingRepoClient{charts: []repoclient.Chart{{
		Name:     "podinfo",
		Versions: []repoclient.ChartVersion{ociVersion("6.7.1", "application/tar+gzip")},
	}}}

	service, _ := newSynchronizer(t, stubRepoClient{}, repo)
	service.clientFactory = func(_ chartsource.Kind) (repoclient.ClientInterface, error) {
		return stub, nil
	}

	service.Sync(context.Background(), adapter.NewAddonRepository(repo), chartsource.OCI)

	if !stub.opts.Full {
		t.Fatal("force reconcile must request a full re-index")
	}
}

// TestSynchronizerSyncRecordsAForcedRefreshBeforeTheRegistryIsRead covers the failure
// a forced refresh used to disappear into. The annotation is consumed by the pass that
// attempts the read, so if the read is all that carries the request, a registry that is
// briefly unreachable ends the refresh: the annotation is gone, no version is marked,
// and every pass after that skips the versions whose media type is recorded — including
// the re-pushed tag the refresh was asked for — while the repository reports one
// successful synchronization after another. The request is written into the catalog
// before the registry is contacted, so the failed read costs the refresh nothing.
func TestSynchronizerSyncRecordsAForcedRefreshBeforeTheRegistryIsRead(t *testing.T) {
	const mediaType = "application/vnd.cncf.helm.chart.content.v1.tar+gzip"

	forced := testRepository()
	forced.Annotations = map[string]string{helmv1alpha1.AnnotationForceReconcile: ""}
	chart := existingChart(forced.Name, "podinfo",
		helmv1alpha1.ChartVersion{Version: "6.7.1", MediaType: mediaType},
	)

	service, c := newSynchronizer(t, stubRepoClient{err: errors.New("dial tcp: connection refused")}, forced, chart)

	outcome := service.Sync(context.Background(), adapter.NewAddonRepository(forced), chartsource.OCI)
	if outcome.Fetch.Err == nil {
		t.Fatal("the fixture must reach a failed forced read")
	}
	if !outcome.ForceRequestRecorded {
		t.Fatal("the request was recorded before the read: the caller may consume the annotation")
	}

	marked := chartStatus(t, c, forced.Name, "podinfo").Versions
	if len(marked) != 1 || !marked[0].ReexaminationRequired {
		t.Fatalf("recorded versions = %+v, want the forced refresh written onto them", marked)
	}

	// The next pass is an ordinary one — the annotation was consumed by the failed pass —
	// and it must still re-examine the version the refresh was asked for.
	ordinary := testRepository()
	stub := &recordingRepoClient{charts: []repoclient.Chart{{
		Name:     "podinfo",
		Versions: []repoclient.ChartVersion{ociVersion("6.7.1", "application/tar+gzip")},
	}}}
	service.clientFactory = func(_ chartsource.Kind) (repoclient.ClientInterface, error) {
		return stub, nil
	}

	if outcome := service.Sync(context.Background(), adapter.NewAddonRepository(ordinary), chartsource.OCI); outcome.Fetch.Err != nil {
		t.Fatalf("the ordinary pass failed: %v", outcome.Fetch.Err)
	}

	if stub.opts.Full {
		t.Fatal("the fixture must drive an ordinary pass, or it proves nothing about the mark")
	}
	if !stub.opts.NeedsExamination("podinfo", "6.7.1") {
		t.Fatal("the pass after a failed forced one must re-examine the recorded version")
	}
	if got := chartStatus(t, c, forced.Name, "podinfo").Versions; len(got) != 1 || got[0].ReexaminationRequired {
		t.Fatalf("versions after the refresh = %+v, want the mark cleared by the pass that answered it", got)
	}
}

// budgetedRepoClient is a repository client with a budget, as the OCI one has: it
// examines at most budget of the versions that still need examining and reports the
// rest as unexamined, carrying forward the recorded verdict of every version it is
// allowed to skip. mediaType is what the registry holds now — every tag was re-pushed —
// so a version that was examined is distinguishable from one that was only carried.
type budgetedRepoClient struct {
	chartName string
	tags      []string
	mediaType string
	budget    int
}

func (b *budgetedRepoClient) FetchCharts(_ context.Context, _ string, _ *repoclient.RepoConfig, opts repoclient.FetchOptions) ([]repoclient.Chart, error) {
	chart := repoclient.Chart{Name: b.chartName}
	left := b.budget

	for _, tag := range b.tags {
		known, recorded := opts.Known[b.chartName][tag]

		switch {
		case !opts.NeedsExamination(b.chartName, tag):
			chart.Versions = append(chart.Versions, repoclient.ChartVersion{
				Version:            semver.MustParse(tag),
				MediaType:          known.MediaType,
				UnavailableReason:  known.UnavailableReason,
				UnavailableMessage: known.UnavailableMessage,
			})
		case left == 0:
			if !recorded {
				// A version nothing is recorded for and nothing was observed about
				// carries no entry at all, exactly as the real client leaves it.
				continue
			}

			chart.UnexaminedVersions = append(chart.UnexaminedVersions, tag)
		default:
			left--

			chart.Versions = append(chart.Versions, repoclient.ChartVersion{
				Version:   semver.MustParse(tag),
				MediaType: b.mediaType,
			})
		}
	}

	return []repoclient.Chart{chart}, nil
}

// TestSynchronizerSyncFinishesAForcedRefreshOverSeveralIncompletePasses runs the whole
// mechanism end to end against the production catalog: a forced pass that does not get
// through the repository, then ordinary passes under the same budget, each rebuilding
// what it knows from the catalog objects the previous one wrote. Every pass but the
// last is incomplete, so the refresh is carried entirely by what the catalog records,
// and it has to end exactly where one unbounded forced pass would have ended: every
// version re-examined, nothing left marked.
func TestSynchronizerSyncFinishesAForcedRefreshOverSeveralIncompletePasses(t *testing.T) {
	const (
		recorded = "application/vnd.cncf.helm.chart.content.v1.tar+gzip"
		repushed = "application/tar+gzip"
	)

	tags := []string{"6.7.2", "6.7.1", "6.7.0"}

	forced := testRepository()
	forced.Annotations = map[string]string{helmv1alpha1.AnnotationForceReconcile: ""}

	versions := make([]helmv1alpha1.ChartVersion, 0, len(tags))
	for _, tag := range tags {
		versions = append(versions, helmv1alpha1.ChartVersion{Version: tag, MediaType: recorded})
	}

	service, c := newSynchronizer(t, stubRepoClient{}, forced, existingChart(forced.Name, "podinfo", versions...))
	// One version per pass: three passes are needed, and none of them sees the whole
	// repository.
	service.clientFactory = func(_ chartsource.Kind) (repoclient.ClientInterface, error) {
		return &budgetedRepoClient{chartName: "podinfo", tags: tags, mediaType: repushed, budget: 1}, nil
	}

	ordinary := testRepository()
	passes := []struct {
		name string
		repo *helmv1alpha1.HelmClusterAddonRepository
		// examined is how many versions carry the re-pushed media type once the pass
		// has been written into the catalog.
		examined int
	}{
		{name: "the forced pass", repo: forced, examined: 1},
		{name: "the first ordinary pass after it", repo: ordinary, examined: 2},
		{name: "the pass that finishes the refresh", repo: ordinary, examined: 3},
	}

	for _, pass := range passes {
		outcome := service.Sync(context.Background(), adapter.NewAddonRepository(pass.repo), chartsource.OCI)
		if outcome.Fetch.Err != nil {
			t.Fatalf("%s failed: %v", pass.name, outcome.Fetch.Err)
		}
		if outcome.Catalog.Err != nil {
			t.Fatalf("%s failed to write the catalog: %v", pass.name, outcome.Catalog.Err)
		}

		stored := chartStatus(t, c, forced.Name, "podinfo").Versions
		if len(stored) != len(tags) {
			t.Fatalf("%s left %+v, want all %d versions recorded", pass.name, stored, len(tags))
		}

		examined := 0
		for _, version := range stored {
			switch version.MediaType {
			case repushed:
				examined++

				if version.ReexaminationRequired {
					t.Fatalf("%s left %s marked after examining it: the refresh would never end", pass.name, version.Version)
				}
			case recorded:
				if !version.ReexaminationRequired {
					t.Fatalf("%s dropped the refresh %s still owes: no later pass would re-examine it", pass.name, version.Version)
				}
			default:
				t.Fatalf("%s recorded %s with media type %q", pass.name, version.Version, version.MediaType)
			}
		}

		if examined != pass.examined {
			t.Fatalf("%s left %d versions re-examined, want %d: %+v", pass.name, examined, pass.examined, stored)
		}
	}
}

func TestSynchronizerSyncRetainsReferencedVersionRemovedFromRepository(t *testing.T) {
	repo := testRepository()
	chart := existingChart(repo.Name, "podinfo",
		helmv1alpha1.ChartVersion{Version: "6.7.1", MediaType: "application/tar+gzip"},
		helmv1alpha1.ChartVersion{Version: "6.7.0", MediaType: "application/tar+gzip"},
	)
	addon := addonUsing(repo.Name, "podinfo", "6.7.1")

	stub := stubRepoClient{charts: []repoclient.Chart{{
		Name:     "podinfo",
		Versions: []repoclient.ChartVersion{ociVersion("6.8.0", "application/tar+gzip")},
	}}}

	service, c := newSynchronizer(t, stub, repo, chart, addon)
	if outcome := service.Sync(context.Background(), adapter.NewAddonRepository(repo), chartsource.OCI); outcome.Catalog.Err != nil {
		t.Fatalf("catalog update failed: %v", outcome.Catalog.Err)
	}

	status := chartStatus(t, c, repo.Name, "podinfo")

	var retained, pruned bool
	for _, version := range status.Versions {
		switch version.Version {
		case "6.7.1":
			retained = true
			if version.UnavailableReason != helmv1alpha1.UnavailableReasonRemovedFromRepository {
				t.Fatalf("retained version reason is %q", version.UnavailableReason)
			}
			if version.MediaType != "application/tar+gzip" {
				t.Fatalf("retained version lost its media type: %q", version.MediaType)
			}
		case "6.7.0":
			pruned = true
		}
	}

	if !retained {
		t.Fatal("a referenced version must be retained")
	}
	if pruned {
		t.Fatal("an unreferenced version must be pruned")
	}
}

// TestSynchronizerSyncRetainsMediaTypeForReferencedUnsupportedVersion covers D4's fourth row: a
// version whose tag was re-pushed as a non-chart artifact must keep its previously
// recorded media type as long as an addon still references it, so the addon's internal
// OCIRepository can still be built and the pull failure surfaces from the source
// controller instead of the deploy gate. The same version, unreferenced, must not carry
// the old media type forward: retention is scoped to in-use versions only.
func TestSynchronizerSyncRetainsMediaTypeForReferencedUnsupportedVersion(t *testing.T) {
	repo := testRepository()
	chart := existingChart(repo.Name, "podinfo",
		helmv1alpha1.ChartVersion{Version: "6.7.1", MediaType: "application/tar+gzip"},
		helmv1alpha1.ChartVersion{Version: "6.7.0", MediaType: "application/tar+gzip"},
	)
	addon := addonUsing(repo.Name, "podinfo", "6.7.1")

	stub := stubRepoClient{charts: []repoclient.Chart{{
		Name: "podinfo",
		Versions: []repoclient.ChartVersion{
			{
				Version:            semver.MustParse("6.7.1"),
				UnavailableReason:  helmv1alpha1.UnavailableReasonUnsupportedMediaType,
				UnavailableMessage: "layer media type \"application/vnd.oci.image.layer.v1.tar\" is not a helm chart layer",
			},
			{
				Version:            semver.MustParse("6.7.0"),
				UnavailableReason:  helmv1alpha1.UnavailableReasonUnsupportedMediaType,
				UnavailableMessage: "layer media type \"application/vnd.oci.image.layer.v1.tar\" is not a helm chart layer",
			},
		},
	}}}

	service, c := newSynchronizer(t, stub, repo, chart, addon)
	if outcome := service.Sync(context.Background(), adapter.NewAddonRepository(repo), chartsource.OCI); outcome.Catalog.Err != nil {
		t.Fatalf("catalog update failed: %v", outcome.Catalog.Err)
	}

	status := chartStatus(t, c, repo.Name, "podinfo")

	var sawReferenced, sawUnreferenced bool
	for _, version := range status.Versions {
		switch version.Version {
		case "6.7.1":
			sawReferenced = true
			if version.UnavailableReason != helmv1alpha1.UnavailableReasonUnsupportedMediaType {
				t.Fatalf("referenced version reason is %q, want UnsupportedMediaType", version.UnavailableReason)
			}
			if version.MediaType != "application/tar+gzip" {
				t.Fatalf("referenced version lost its media type: %q", version.MediaType)
			}
		case "6.7.0":
			sawUnreferenced = true
			if version.MediaType != "" {
				t.Fatalf("unreferenced version must not carry its old media type forward: %q", version.MediaType)
			}
			if version.UnavailableReason != helmv1alpha1.UnavailableReasonUnsupportedMediaType {
				t.Fatalf("unreferenced version reason is %q, want UnsupportedMediaType", version.UnavailableReason)
			}
		}
	}

	if !sawReferenced {
		t.Fatal("referenced version 6.7.1 must be present")
	}
	if !sawUnreferenced {
		t.Fatal("unreferenced version 6.7.0 must be present")
	}
}

func TestSynchronizerSyncOrdersVersionsBySemverDescending(t *testing.T) {
	repo := testRepository()
	stub := stubRepoClient{charts: []repoclient.Chart{{
		Name: "podinfo",
		Versions: []repoclient.ChartVersion{
			ociVersion("6.7.1", "application/tar+gzip"),
			ociVersion("6.10.0", "application/tar+gzip"),
			ociVersion("6.8.0", "application/tar+gzip"),
		},
	}}}

	service, c := newSynchronizer(t, stub, repo)
	service.Sync(context.Background(), adapter.NewAddonRepository(repo), chartsource.OCI)

	got := chartStatus(t, c, repo.Name, "podinfo").Versions
	want := []string{"6.10.0", "6.8.0", "6.7.1"}
	if len(got) != len(want) {
		t.Fatalf("versions are %v, want %v", got, want)
	}
	for i, version := range want {
		if got[i].Version != version {
			t.Fatalf("versions are %v, want %v", got, want)
		}
	}
}

func TestSynchronizerSyncCreatesChartWithNoUsableVersions(t *testing.T) {
	repo := testRepository()
	stub := stubRepoClient{charts: []repoclient.Chart{{
		Name: "podinfo",
		Versions: []repoclient.ChartVersion{{
			Version:            semver.MustParse("6.7.1"),
			UnavailableReason:  helmv1alpha1.UnavailableReasonResolvePending,
			UnavailableMessage: "registry returned 500",
		}},
	}}}

	service, c := newSynchronizer(t, stub, repo)
	outcome := service.Sync(context.Background(), adapter.NewAddonRepository(repo), chartsource.OCI)

	if outcome.Catalog.Err != nil {
		t.Fatalf("catalog update failed: %v", outcome.Catalog.Err)
	}
	if outcome.Fetch.Pending != 1 {
		t.Fatalf("pending count is %d, want 1", outcome.Fetch.Pending)
	}

	status := chartStatus(t, c, repo.Name, "podinfo")
	if len(status.Versions) != 1 || status.Versions[0].UnavailableReason != helmv1alpha1.UnavailableReasonResolvePending {
		t.Fatalf("chart status is %+v", status.Versions)
	}
}

func TestSynchronizerSyncKeepsChartReferencedByAddon(t *testing.T) {
	repo := testRepository()
	chart := existingChart(repo.Name, "podinfo",
		helmv1alpha1.ChartVersion{Version: "6.7.1", MediaType: "application/tar+gzip"},
	)
	addon := addonUsing(repo.Name, "podinfo", "6.7.1")

	service, c := newSynchronizer(t, stubRepoClient{charts: nil}, repo, chart, addon)
	if outcome := service.Sync(context.Background(), adapter.NewAddonRepository(repo), chartsource.OCI); outcome.Catalog.Err != nil {
		t.Fatalf("catalog update failed: %v", outcome.Catalog.Err)
	}

	key := client.ObjectKey{Name: naming.HelmClusterAddonChartName(repo.Name, "podinfo")}
	if err := c.Get(context.Background(), key, &helmv1alpha1.HelmClusterAddonChart{}); err != nil {
		t.Fatalf("a chart referenced by an addon must not be deleted: %v", err)
	}
}

// TestSynchronizerSyncReportsNoFetchAttemptOnClusterReadFailure covers the knownCharts failure
// path: the registry is never contacted, so the outcome must say so. Reporting
// FetchAttempted: true here (or a zero-value Fetch with Err == nil) would make a
// cluster-side read failure look like a successful repository fetch to a caller
// that only checks Fetch.Err.
func TestSynchronizerSyncReportsNoFetchAttemptOnClusterReadFailure(t *testing.T) {
	repo := testRepository()
	scheme := testScheme(t)

	sentinel := errors.New("synthetic chart list failure")
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(repo).
		WithStatusSubresource(&helmv1alpha1.HelmClusterAddonChart{}, &helmv1alpha1.HelmClusterAddonRepository{}).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, wc client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if _, ok := list.(*helmv1alpha1.HelmClusterAddonChartList); ok {
					return sentinel
				}

				return wc.List(ctx, list, opts...)
			},
		}).
		Build()

	factory := func(_ chartsource.Kind) (repoclient.ClientInterface, error) {
		return stubRepoClient{}, nil
	}
	service := NewSynchronizer(c, factory, adapter.NewAddonCatalog(c))

	outcome := service.Sync(context.Background(), adapter.NewAddonRepository(repo), chartsource.OCI)

	if outcome.FetchAttempted {
		t.Fatal("a cluster-side read failure before the fetch must not report FetchAttempted")
	}
	if outcome.Fetch.Err != nil {
		t.Fatalf("Fetch must stay zero-valued on this path, got Err: %v", outcome.Fetch.Err)
	}
	if outcome.Catalog.Err == nil {
		t.Fatal("expected a catalog error from the failed chart listing")
	}
	if !errors.Is(outcome.Catalog.Err, sentinel) {
		t.Fatalf("catalog error must wrap the underlying failure, got %v", outcome.Catalog.Err)
	}
}

// TestSynchronizerSyncReportsTheVersionsAPassLeftUnexamined pins the signal that
// travels from the client to the reconciler. A pass that ran out of its budget did
// publish a catalog, so it is not a fetch failure, and it did not finish, so it must
// not read as a completed synchronization either: the count is what the reconciler
// schedules the next pass by.
func TestSynchronizerSyncReportsTheVersionsAPassLeftUnexamined(t *testing.T) {
	repo := testRepository()
	incomplete := chartFixture("podinfo", "6.7.1")
	incomplete.UnexaminedVersions = []string{"6.7.0", "6.6.0"}

	service, _ := newSynchronizer(t, stubRepoClient{charts: []repoclient.Chart{incomplete}}, repo)

	outcome := service.Sync(context.Background(), adapter.NewAddonRepository(repo), chartsource.Helm)
	if outcome.Fetch.Err != nil {
		t.Fatalf("an incomplete pass is not a failed fetch, got %v", outcome.Fetch.Err)
	}
	if outcome.Catalog.Err != nil {
		t.Fatalf("catalog update failed: %v", outcome.Catalog.Err)
	}
	if outcome.Fetch.Unexamined != 2 {
		t.Fatalf("unexamined = %d, want 2", outcome.Fetch.Unexamined)
	}
	if outcome.Fetch.Pending != 0 {
		t.Fatalf("pending = %d: an unexamined version is not a pending verdict", outcome.Fetch.Pending)
	}
}
