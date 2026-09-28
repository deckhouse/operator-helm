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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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

func newSynchronizer(t *testing.T, stub repoclient.ClientInterface, objects ...client.Object) (*Synchronizer, client.Client) {
	t.Helper()

	return newSynchronizerWithInterceptor(t, stub, interceptor.Funcs{}, objects...)
}

func newSynchronizerWithInterceptor(
	t *testing.T,
	stub repoclient.ClientInterface,
	funcs interceptor.Funcs,
	objects ...client.Object,
) (*Synchronizer, client.Client) {
	t.Helper()

	scheme := testScheme(t)
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objects...).
		WithInterceptorFuncs(funcs).
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

	return NewSynchronizer(c, factory, adapter.NewAddonCatalog(c, c)), c
}

// newSynchronizerWithLaggingCache wires the synchronization the way the manager does,
// with the two readers kept apart: writes and cached reads go through the intercepted
// client, while the catalog reads the store directly, as it does through the API reader
// in production. funcs therefore describe an informer that has not caught up — and
// nothing else — so a test can state exactly what the lag would cost. The store is
// returned, because what a pass wrote is a question about the cluster, not about the
// cache in front of it.
func newSynchronizerWithLaggingCache(
	t *testing.T,
	stub repoclient.ClientInterface,
	funcs interceptor.Funcs,
	objects ...client.Object,
) (*Synchronizer, client.Client) {
	t.Helper()

	scheme := testScheme(t)
	store := fake.NewClientBuilder().
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

	cached := interceptor.NewClient(store, funcs)

	factory := func(_ chartsource.Kind) (repoclient.ClientInterface, error) {
		return stub, nil
	}

	return NewSynchronizer(cached, factory, adapter.NewAddonCatalog(cached, store)), store
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

// TestSynchronizerSyncRecordsAForcedRefreshOnlyWhereItMeansSomething pins which
// repositories pay for the durable form of the request. The mark exists to stop a later
// pass from skipping a recorded verdict, and only the OCI client skips anything: the
// helm client ignores FetchOptions outright, because the index is one document carrying
// every version and media types are not probed incrementally there. Marking a helm
// repository would rewrite the whole version list of every chart the index publishes —
// hundreds of status writes on a large index, and as many again to clear them — where an
// unchanged index produces no status diff at all, and an index fetch that failed would
// leave every version of every chart claiming a re-examination no pass is going to do.
//
// The request counts as recorded either way: a repository with nothing to record has
// nothing left waiting on the annotation, and keeping it would force the repository on
// every pass forever.
func TestSynchronizerSyncRecordsAForcedRefreshOnlyWhereItMeansSomething(t *testing.T) {
	const mediaType = "application/vnd.cncf.helm.chart.content.v1.tar+gzip"

	tests := []struct {
		name     string
		repoType chartsource.Kind
		wantMark bool
	}{
		{name: "an oci repository, whose passes skip recorded verdicts", repoType: chartsource.OCI, wantMark: true},
		{name: "a helm repository, whose client ignores what is recorded", repoType: chartsource.Helm, wantMark: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			forced := testRepository()
			forced.Annotations = map[string]string{helmv1alpha1.AnnotationForceReconcile: ""}
			chart := existingChart(forced.Name, "podinfo",
				helmv1alpha1.ChartVersion{Version: "6.7.1", MediaType: mediaType},
			)

			patches := 0
			// The read fails, so the only writes this pass can make are the marking ones:
			// the catalog is never reconciled, and every patch counted here is the request
			// being written down.
			service, c := newSynchronizerWithInterceptor(t,
				stubRepoClient{err: errors.New("dial tcp: connection refused")},
				interceptor.Funcs{
					SubResourcePatch: func(
						ctx context.Context,
						cl client.Client,
						sub string,
						obj client.Object,
						patch client.Patch,
						opts ...client.SubResourcePatchOption,
					) error {
						if _, ok := obj.(*helmv1alpha1.HelmClusterAddonChart); ok {
							patches++
						}

						return cl.SubResource(sub).Patch(ctx, obj, patch, opts...)
					},
				}, forced, chart)

			outcome := service.Sync(context.Background(), adapter.NewAddonRepository(forced), test.repoType)
			if outcome.Fetch.Err == nil {
				t.Fatal("the fixture must reach a failed forced read")
			}
			if !outcome.ForceRequestRecorded {
				t.Fatal("there is nothing left to record: the caller must be free to consume the annotation")
			}

			wantPatches := 0
			if test.wantMark {
				wantPatches = 1
			}
			if patches != wantPatches {
				t.Fatalf("chart status patches = %d, want %d", patches, wantPatches)
			}

			versions := chartStatus(t, c, forced.Name, "podinfo").Versions
			if len(versions) != 1 {
				t.Fatalf("recorded versions = %+v, want the fixture's single version", versions)
			}
			if versions[0].ReexaminationRequired != test.wantMark {
				t.Fatalf("ReexaminationRequired = %t, want %t", versions[0].ReexaminationRequired, test.wantMark)
			}
		})
	}
}

// TestSynchronizerSyncKeepsAForcedRefreshWhenTheCatalogCannotBeRead covers the failure
// that comes before anything is written. A pass reads the catalog once, and a forced
// pass writes its request onto what that read returned; a read that fails has therefore
// recorded nothing, and the annotation is the only thing left carrying the refresh.
// Reporting it as recorded there would consume it for work no pass has done.
//
// A repository nothing is marked for is the other case: there was never anything to
// write down, so the failed read holds nothing up and keeping the annotation would force
// the repository again on every pass forever.
func TestSynchronizerSyncKeepsAForcedRefreshWhenTheCatalogCannotBeRead(t *testing.T) {
	const mediaType = "application/vnd.cncf.helm.chart.content.v1.tar+gzip"

	tests := []struct {
		name         string
		repoType     chartsource.Kind
		wantRecorded bool
	}{
		{
			name:         "an oci repository, whose request is written onto the read that failed",
			repoType:     chartsource.OCI,
			wantRecorded: false,
		},
		{
			name:         "a helm repository, which records nothing either way",
			repoType:     chartsource.Helm,
			wantRecorded: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			forced := testRepository()
			forced.Annotations = map[string]string{helmv1alpha1.AnnotationForceReconcile: ""}
			chart := existingChart(forced.Name, "podinfo",
				helmv1alpha1.ChartVersion{Version: "6.7.1", MediaType: mediaType},
			)

			sentinel := errors.New("synthetic chart list failure")
			service, c := newSynchronizerWithInterceptor(t, stubRepoClient{}, interceptor.Funcs{
				List: func(ctx context.Context, wc client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
					if _, ok := list.(*helmv1alpha1.HelmClusterAddonChartList); ok {
						return sentinel
					}

					return wc.List(ctx, list, opts...)
				},
			}, forced, chart)

			outcome := service.Sync(context.Background(), adapter.NewAddonRepository(forced), test.repoType)

			if !errors.Is(outcome.Catalog.Err, sentinel) {
				t.Fatalf("the fixture must fail the catalog read, got %v", outcome.Catalog.Err)
			}
			if outcome.FetchAttempted {
				t.Fatal("the registry is never contacted once the catalog cannot be read")
			}
			if outcome.ForceRequestRecorded != test.wantRecorded {
				t.Fatalf("ForceRequestRecorded = %t, want %t", outcome.ForceRequestRecorded, test.wantRecorded)
			}

			versions := chartStatus(t, c, forced.Name, "podinfo").Versions
			if len(versions) != 1 || versions[0].ReexaminationRequired {
				t.Fatalf("recorded versions = %+v, want nothing written by a pass that could not read", versions)
			}
		})
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

// TestSynchronizerSyncExaminesAMarkedVersionAndClearsItByAnswering pins what an
// ordinary pass does with a mark a forced pass left behind: the annotation was consumed
// by that forced pass, so the mark is the whole of the request, and the verdicts this
// pass skips tags by come from the same read the mark is on. NeedsExamination therefore
// sees it, the version is probed rather than carried, and the answer clears the mark —
// which is what keeps a refresh from repeating forever.
func TestSynchronizerSyncExaminesAMarkedVersionAndClearsItByAnswering(t *testing.T) {
	const (
		recorded = "application/vnd.cncf.helm.chart.content.v1.tar+gzip"
		repushed = "application/tar+gzip"
	)

	repo := testRepository()
	marked := existingChart(repo.Name, "podinfo",
		helmv1alpha1.ChartVersion{Version: "6.7.1", MediaType: recorded, ReexaminationRequired: true},
	)

	// The repository carries no force annotation: this is an ordinary pass, and it
	// marks nothing itself.
	service, c := newSynchronizer(t,
		&budgetedRepoClient{chartName: "podinfo", tags: []string{"6.7.1"}, mediaType: repushed, budget: 1},
		repo, marked)

	outcome := service.Sync(context.Background(), adapter.NewAddonRepository(repo), chartsource.OCI)
	if outcome.Fetch.Err != nil {
		t.Fatalf("fetch failed: %v", outcome.Fetch.Err)
	}
	if outcome.Catalog.Err != nil {
		t.Fatalf("catalog update failed: %v", outcome.Catalog.Err)
	}

	stored := chartStatus(t, c, repo.Name, "podinfo").Versions
	if len(stored) != 1 || stored[0].Version != "6.7.1" {
		t.Fatalf("recorded versions = %+v, want the single version the fixture records", stored)
	}
	if stored[0].MediaType != repushed {
		t.Fatalf("media type = %q, want %q: a marked version must be examined, not carried", stored[0].MediaType, repushed)
	}
	if stored[0].ReexaminationRequired {
		t.Fatal("a version this pass examined must have its mark cleared: the refresh has been answered")
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
	service := NewSynchronizer(c, factory, adapter.NewAddonCatalog(c, c))

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

// laggingChartCache builds the interceptor of a cache that has not caught up: every
// chart object a cached read returns is the one the store holds, rewritten by mutate
// into the state an older observation of it was in. Both reads are covered, because a
// cache lags in all of them and a fixture that lagged in only one would be describing a
// cache that does not exist.
func laggingChartCache(mutate func(*helmv1alpha1.HelmClusterAddonChart)) interceptor.Funcs {
	return interceptor.Funcs{
		Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if err := cl.Get(ctx, key, obj, opts...); err != nil {
				return err
			}

			if chart, ok := obj.(*helmv1alpha1.HelmClusterAddonChart); ok {
				mutate(chart)
			}

			return nil
		},
		List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if err := cl.List(ctx, list, opts...); err != nil {
				return err
			}

			charts, ok := list.(*helmv1alpha1.HelmClusterAddonChartList)
			if !ok {
				return nil
			}

			for i := range charts.Items {
				mutate(&charts.Items[i])
			}

			return nil
		},
	}
}

// TestASecondForceIsRecordedWhenTheCachedCatalogStillShowsTheFirst covers the force that
// is consumed having recorded nothing. Marking skips the objects that already carry the
// mark everywhere, which is right — a refresh asked for twice over an unreachable
// registry must not rewrite the catalog twice — but only if "already marked" is a
// statement about the catalog. Decided from a cached read it is not: the cache here
// still holds the object from while the first refresh was outstanding, while a pass that
// has since finished answered it and cleared the mark on the server.
//
// A pass that took that read at its word would write nothing, and the annotation is
// consumed by this very pass (SyncOutcome.ForceRequestRecorded): with the fetch then
// failing, the second refresh would be gone with nothing anywhere recording that it was
// ever asked for, and every later pass would go on skipping the recorded verdict.
func TestASecondForceIsRecordedWhenTheCachedCatalogStillShowsTheFirst(t *testing.T) {
	const mediaType = "application/vnd.cncf.helm.chart.content.v1.tar+gzip"

	forced := testRepository()
	forced.Annotations = map[string]string{helmv1alpha1.AnnotationForceReconcile: ""}

	// The store: the first refresh has been answered and the mark cleared.
	answered := existingChart(forced.Name, "podinfo",
		helmv1alpha1.ChartVersion{Version: "6.7.1", MediaType: mediaType},
	)

	// The cache: the object as it was while the first refresh was still outstanding.
	stale := laggingChartCache(func(chart *helmv1alpha1.HelmClusterAddonChart) {
		for i := range chart.Status.Versions {
			chart.Status.Versions[i].ReexaminationRequired = true
		}
	})

	// The registry is unreachable, which is the case the durable record exists for: the
	// pass ends having written the request and nothing else.
	service, store := newSynchronizerWithLaggingCache(t,
		stubRepoClient{err: errors.New("dial tcp: connection refused")}, stale, forced, answered)

	outcome := service.Sync(context.Background(), adapter.NewAddonRepository(forced), chartsource.OCI)
	if outcome.Fetch.Err == nil {
		t.Fatal("the fixture must reach a forced pass whose read failed, or the request would be answered rather than recorded")
	}
	if !outcome.ForceRequestRecorded {
		t.Fatal("the pass reports the request as recorded, so the annotation is consumed: what it wrote has to be in the catalog")
	}

	versions := chartStatus(t, store, forced.Name, "podinfo").Versions
	if len(versions) != 1 {
		t.Fatalf("recorded versions = %+v, want the fixture's single version", versions)
	}
	if !versions[0].ReexaminationRequired {
		t.Fatal("the second force must be written onto the catalog: a cached read showing the first one's mark is not evidence about the catalog, and the annotation is already consumed")
	}
}

// TestAVerdictCarriedByAPassCannotOverwriteANewerOne covers the other half of the same
// rule, on a pass that writes successfully. The verdicts a fetch skips tags by and the
// object the write is patched against have to be one read: a pass that took them
// separately could carry a verdict forward from the older of the two and publish it over
// the newer one, with the optimistic lock powerless to stop it because the object it was
// patched against is current.
//
// Here the catalog holds the media type a completed pass established, while the cache
// still shows the one it replaced. The pass asks the registry nothing about the version —
// its verdict is recorded and unmarked — so whatever it carries is what the catalog ends
// up publishing, and an ordinary pass would go on skipping the tag forever.
func TestAVerdictCarriedByAPassCannotOverwriteANewerOne(t *testing.T) {
	const (
		established = "application/tar+gzip"
		replaced    = "application/vnd.cncf.helm.chart.content.v1.tar+gzip"
	)

	repo := testRepository()
	current := existingChart(repo.Name, "podinfo",
		helmv1alpha1.ChartVersion{Version: "6.7.1", MediaType: established},
	)

	// The cache: the object as it was before the pass that established the media type.
	stale := laggingChartCache(func(chart *helmv1alpha1.HelmClusterAddonChart) {
		for i := range chart.Status.Versions {
			chart.Status.Versions[i].MediaType = replaced
		}
	})

	service, store := newSynchronizerWithLaggingCache(t,
		&budgetedRepoClient{chartName: "podinfo", tags: []string{"6.7.1"}, mediaType: "application/gzip", budget: 1},
		stale, repo, current)

	outcome := service.Sync(context.Background(), adapter.NewAddonRepository(repo), chartsource.OCI)
	if outcome.Fetch.Err != nil {
		t.Fatalf("fetch failed: %v", outcome.Fetch.Err)
	}
	if outcome.Catalog.Err != nil {
		t.Fatalf("catalog update failed: %v", outcome.Catalog.Err)
	}

	versions := chartStatus(t, store, repo.Name, "podinfo").Versions
	if len(versions) != 1 {
		t.Fatalf("recorded versions = %+v, want the fixture's single version", versions)
	}
	if versions[0].MediaType == "application/gzip" {
		t.Fatal("the fixture must reach a carried verdict, not a probe: a version whose verdict is recorded and unmarked is exactly what a pass skips")
	}
	if versions[0].MediaType != established {
		t.Fatalf("media type = %q, want %q: a verdict carried forward must be the one the catalog holds, not one a cached read remembered",
			versions[0].MediaType, established)
	}
}

// TestSynchronizerSyncRejectsACatalogWriteOvertakenByAnotherWriter covers the window
// the optimistic lock exists for. A pass reads the catalog once and writes it at the
// end, and between the two lies the whole fetch — long enough for another writer to
// record something this pass knows nothing about. A merge patch on a list ships the
// whole array, so the write would replace that state wholesale with one computed
// without it.
//
// What is lost is worst for a mark: the writer here is a forced pass that recorded a
// refresh and had its annotation consumed for it, so the mark is the only record of the
// request. Under the lock the base carries the resource version it was read at, the
// server rejects the patch as a conflict, the pass reports it, and the retry — computed
// from a read taken after the mark — examines exactly the version it asks about and
// clears it by answering.
func TestSynchronizerSyncRejectsACatalogWriteOvertakenByAnotherWriter(t *testing.T) {
	const (
		recorded = "application/vnd.cncf.helm.chart.content.v1.tar+gzip"
		repushed = "application/tar+gzip"
	)

	repo := testRepository()
	chart := existingChart(repo.Name, "podinfo",
		helmv1alpha1.ChartVersion{Version: "6.7.1", MediaType: recorded},
	)

	// The competing writer runs once, just before this pass's own status patch: the
	// object it marks is the one the pass read before its fetch, and everything the
	// pass is about to write was computed from that earlier state.
	overtaken := false
	funcs := interceptor.Funcs{
		SubResourcePatch: func(
			ctx context.Context,
			cl client.Client,
			sub string,
			obj client.Object,
			patch client.Patch,
			opts ...client.SubResourcePatchOption,
		) error {
			if _, ok := obj.(*helmv1alpha1.HelmClusterAddonChart); ok && !overtaken {
				overtaken = true

				marked := &helmv1alpha1.HelmClusterAddonChart{}
				if err := cl.Get(ctx, client.ObjectKeyFromObject(obj), marked); err != nil {
					return err
				}

				for i := range marked.Status.Versions {
					marked.Status.Versions[i].ReexaminationRequired = true
				}

				if err := cl.Status().Update(ctx, marked); err != nil {
					return err
				}
			}

			return cl.SubResource(sub).Patch(ctx, obj, patch, opts...)
		},
	}

	// The registry published one more tag, so the array this pass computed differs from
	// what it read and the patch is a real write rather than a no-op.
	client := &budgetedRepoClient{
		chartName: "podinfo",
		tags:      []string{"6.7.2", "6.7.1"},
		mediaType: repushed,
		budget:    2,
	}
	service, c := newSynchronizerWithInterceptor(t, client, funcs, repo, chart)
	addonRepo := adapter.NewAddonRepository(repo)

	overtakenPass := service.Sync(context.Background(), addonRepo, chartsource.OCI)
	if overtakenPass.Fetch.Err != nil {
		t.Fatalf("the fetch of the overtaken pass failed: %v", overtakenPass.Fetch.Err)
	}
	if !apierrors.IsConflict(overtakenPass.Catalog.Err) {
		t.Fatalf("catalog error = %v, want the patch rejected as a conflict: the read it was computed from has been overtaken", overtakenPass.Catalog.Err)
	}

	survived := versionsByVersion(t, chartStatus(t, c, repo.Name, "podinfo").Versions)
	if len(survived) != 1 || !survived["6.7.1"].ReexaminationRequired {
		t.Fatalf("recorded versions = %+v, want the mark untouched by the rejected write", survived)
	}
	if got := survived["6.7.1"].MediaType; got != recorded {
		t.Fatalf("6.7.1 media type = %q, want %q: a rejected patch must leave the catalog as it was", got, recorded)
	}

	// The retry the work queue runs: its read is taken after the mark, so the pass sees
	// it, examines the version it asks about and answers it.
	retried := service.Sync(context.Background(), addonRepo, chartsource.OCI)
	if retried.Fetch.Err != nil || retried.Catalog.Err != nil {
		t.Fatalf("the retry returned fetch %v, catalog %v: a read nothing has overtaken must be writable", retried.Fetch.Err, retried.Catalog.Err)
	}

	written := versionsByVersion(t, chartStatus(t, c, repo.Name, "podinfo").Versions)
	if len(written) != 2 {
		t.Fatalf("recorded versions = %+v, want both tags published", written)
	}
	if written["6.7.1"].ReexaminationRequired {
		t.Fatal("the pass that examined the marked version must clear the mark: the refresh has been answered")
	}
	if got := written["6.7.1"].MediaType; got != repushed {
		t.Fatalf("6.7.1 media type = %q, want %q: the mark survived to be answered by a probe", got, repushed)
	}
}

func versionsByVersion(t *testing.T, versions []helmv1alpha1.ChartVersion) map[string]helmv1alpha1.ChartVersion {
	t.Helper()

	byVersion := make(map[string]helmv1alpha1.ChartVersion, len(versions))
	for _, version := range versions {
		byVersion[version.Version] = version
	}

	return byVersion
}
