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
	"errors"
	"testing"

	"github.com/Masterminds/semver/v3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/deckhouse/operator-helm/api/naming"
	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
	repoclient "github.com/deckhouse/operator-helm/internal/client/repository"
	"github.com/deckhouse/operator-helm/internal/index"
	"github.com/deckhouse/operator-helm/internal/utils"
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

func newRepoSyncService(t *testing.T, stub stubRepoClient, objects ...client.Object) (*RepoSyncService, client.Client) {
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

	factory := func(_ utils.InternalRepositoryType) (repoclient.ClientInterface, error) {
		return stub, nil
	}

	return NewRepoSyncService(c, scheme, factory), c
}

func ociVersion(version, mediaType string) repoclient.ChartVersion {
	return repoclient.ChartVersion{Version: semver.MustParse(version), MediaType: mediaType}
}

func existingChart(repoName, chartName string, versions ...helmv1alpha1.HelmClusterAddonChartVersion) *helmv1alpha1.HelmClusterAddonChart {
	return &helmv1alpha1.HelmClusterAddonChart{
		ObjectMeta: metav1.ObjectMeta{
			Name:   naming.HelmClusterAddonChartName(repoName, chartName),
			Labels: map[string]string{LabelRepositoryName: repoName, LabelChartName: chartName},
		},
		Status: helmv1alpha1.HelmClusterAddonChartStatus{Versions: versions},
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

func chartStatus(t *testing.T, c client.Client, repoName, chartName string) helmv1alpha1.HelmClusterAddonChartStatus {
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

func TestSyncCreatesChartsAndRecordsVersions(t *testing.T) {
	repo := testRepository()
	service, c := newRepoSyncService(t, stubRepoClient{charts: []repoclient.Chart{chartFixture("podinfo", "6.7.1")}}, repo)

	outcome := service.Sync(context.Background(), repo, utils.InternalHelmRepository)
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

func TestSyncPrunesStaleCharts(t *testing.T) {
	repo := testRepository()
	stale := &helmv1alpha1.HelmClusterAddonChart{
		ObjectMeta: metav1.ObjectMeta{
			Name:   naming.HelmClusterAddonChartName(repo.Name, "removed"),
			Labels: map[string]string{LabelRepositoryName: repo.Name, LabelChartName: "removed"},
		},
	}

	service, c := newRepoSyncService(t, stubRepoClient{charts: []repoclient.Chart{chartFixture("podinfo", "6.7.1")}}, repo, stale)

	outcome := service.Sync(context.Background(), repo, utils.InternalHelmRepository)
	if outcome.Catalog.Err != nil {
		t.Fatalf("catalog update failed: %v", outcome.Catalog.Err)
	}

	err := c.Get(context.Background(), client.ObjectKeyFromObject(stale), &helmv1alpha1.HelmClusterAddonChart{})
	if err == nil {
		t.Fatal("stale chart must be pruned")
	}
}

func TestSyncReportsTerminalFetchFailure(t *testing.T) {
	repo := testRepository()
	terminal := &repoclient.TerminalError{
		Reason:  helmv1alpha1.ReasonAuthenticationFailed,
		Message: "repository rejected the credentials (HTTP 401)",
	}

	service, _ := newRepoSyncService(t, stubRepoClient{err: terminal}, repo)

	outcome := service.Sync(context.Background(), repo, utils.InternalHelmRepository)
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

func TestSyncReportsTransientFetchFailure(t *testing.T) {
	repo := testRepository()
	service, _ := newRepoSyncService(t, stubRepoClient{err: errors.New("connection refused")}, repo)

	outcome := service.Sync(context.Background(), repo, utils.InternalHelmRepository)
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

func TestSyncPassesKnownVersionsToTheClient(t *testing.T) {
	repo := testRepository()
	chart := existingChart(repo.Name, "podinfo",
		helmv1alpha1.HelmClusterAddonChartVersion{Version: "6.7.1", MediaType: "application/tar+gzip"},
		helmv1alpha1.HelmClusterAddonChartVersion{
			Version:            "6.7.2",
			UnavailableReason:  helmv1alpha1.UnavailableReasonUnsupportedMediaType,
			UnavailableMessage: "layer media type application/vnd.example is not a chart",
		},
	)

	stub := &recordingRepoClient{charts: []repoclient.Chart{{
		Name:     "podinfo",
		Versions: []repoclient.ChartVersion{ociVersion("6.7.1", "application/tar+gzip")},
	}}}

	service, _ := newRepoSyncService(t, stubRepoClient{}, repo, chart)
	service.clientFactory = func(_ utils.InternalRepositoryType) (repoclient.ClientInterface, error) {
		return stub, nil
	}

	if outcome := service.Sync(context.Background(), repo, utils.InternalOCIRepository); outcome.Fetch.Err != nil {
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

func TestSyncRequestsFullPassOnForceReconcile(t *testing.T) {
	repo := testRepository()
	repo.Annotations = map[string]string{helmv1alpha1.AnnotationForceReconcile: ""}

	stub := &recordingRepoClient{charts: []repoclient.Chart{{
		Name:     "podinfo",
		Versions: []repoclient.ChartVersion{ociVersion("6.7.1", "application/tar+gzip")},
	}}}

	service, _ := newRepoSyncService(t, stubRepoClient{}, repo)
	service.clientFactory = func(_ utils.InternalRepositoryType) (repoclient.ClientInterface, error) {
		return stub, nil
	}

	service.Sync(context.Background(), repo, utils.InternalOCIRepository)

	if !stub.opts.Full {
		t.Fatal("force reconcile must request a full re-index")
	}
}

func TestSyncRetainsReferencedVersionRemovedFromRepository(t *testing.T) {
	repo := testRepository()
	chart := existingChart(repo.Name, "podinfo",
		helmv1alpha1.HelmClusterAddonChartVersion{Version: "6.7.1", MediaType: "application/tar+gzip"},
		helmv1alpha1.HelmClusterAddonChartVersion{Version: "6.7.0", MediaType: "application/tar+gzip"},
	)
	addon := addonUsing(repo.Name, "podinfo", "6.7.1")

	stub := stubRepoClient{charts: []repoclient.Chart{{
		Name:     "podinfo",
		Versions: []repoclient.ChartVersion{ociVersion("6.8.0", "application/tar+gzip")},
	}}}

	service, c := newRepoSyncService(t, stub, repo, chart, addon)
	if outcome := service.Sync(context.Background(), repo, utils.InternalOCIRepository); outcome.Catalog.Err != nil {
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

// TestSyncRetainsMediaTypeForReferencedUnsupportedVersion covers D4's fourth row: a
// version whose tag was re-pushed as a non-chart artifact must keep its previously
// recorded media type as long as an addon still references it, so the addon's internal
// OCIRepository can still be built and the pull failure surfaces from the source
// controller instead of the deploy gate. The same version, unreferenced, must not carry
// the old media type forward: retention is scoped to in-use versions only.
func TestSyncRetainsMediaTypeForReferencedUnsupportedVersion(t *testing.T) {
	repo := testRepository()
	chart := existingChart(repo.Name, "podinfo",
		helmv1alpha1.HelmClusterAddonChartVersion{Version: "6.7.1", MediaType: "application/tar+gzip"},
		helmv1alpha1.HelmClusterAddonChartVersion{Version: "6.7.0", MediaType: "application/tar+gzip"},
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

	service, c := newRepoSyncService(t, stub, repo, chart, addon)
	if outcome := service.Sync(context.Background(), repo, utils.InternalOCIRepository); outcome.Catalog.Err != nil {
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

func TestSyncOrdersVersionsBySemverDescending(t *testing.T) {
	repo := testRepository()
	stub := stubRepoClient{charts: []repoclient.Chart{{
		Name: "podinfo",
		Versions: []repoclient.ChartVersion{
			ociVersion("6.7.1", "application/tar+gzip"),
			ociVersion("6.10.0", "application/tar+gzip"),
			ociVersion("6.8.0", "application/tar+gzip"),
		},
	}}}

	service, c := newRepoSyncService(t, stub, repo)
	service.Sync(context.Background(), repo, utils.InternalOCIRepository)

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

func TestSyncCreatesChartWithNoUsableVersions(t *testing.T) {
	repo := testRepository()
	stub := stubRepoClient{charts: []repoclient.Chart{{
		Name: "podinfo",
		Versions: []repoclient.ChartVersion{{
			Version:            semver.MustParse("6.7.1"),
			UnavailableReason:  helmv1alpha1.UnavailableReasonResolvePending,
			UnavailableMessage: "registry returned 500",
		}},
	}}}

	service, c := newRepoSyncService(t, stub, repo)
	outcome := service.Sync(context.Background(), repo, utils.InternalOCIRepository)

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

func TestSyncKeepsChartReferencedByAddon(t *testing.T) {
	repo := testRepository()
	chart := existingChart(repo.Name, "podinfo",
		helmv1alpha1.HelmClusterAddonChartVersion{Version: "6.7.1", MediaType: "application/tar+gzip"},
	)
	addon := addonUsing(repo.Name, "podinfo", "6.7.1")

	service, c := newRepoSyncService(t, stubRepoClient{charts: nil}, repo, chart, addon)
	if outcome := service.Sync(context.Background(), repo, utils.InternalOCIRepository); outcome.Catalog.Err != nil {
		t.Fatalf("catalog update failed: %v", outcome.Catalog.Err)
	}

	key := client.ObjectKey{Name: naming.HelmClusterAddonChartName(repo.Name, "podinfo")}
	if err := c.Get(context.Background(), key, &helmv1alpha1.HelmClusterAddonChart{}); err != nil {
		t.Fatalf("a chart referenced by an addon must not be deleted: %v", err)
	}
}

// TestSyncReportsNoFetchAttemptOnClusterReadFailure covers the knownCharts failure
// path: the registry is never contacted, so the outcome must say so. Reporting
// FetchAttempted: true here (or a zero-value Fetch with Err == nil) would make a
// cluster-side read failure look like a successful repository fetch to a caller
// that only checks Fetch.Err.
func TestSyncReportsNoFetchAttemptOnClusterReadFailure(t *testing.T) {
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

	factory := func(_ utils.InternalRepositoryType) (repoclient.ClientInterface, error) {
		return stubRepoClient{}, nil
	}
	service := NewRepoSyncService(c, scheme, factory)

	outcome := service.Sync(context.Background(), repo, utils.InternalOCIRepository)

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

// TestMergeChartVersionsCarriesOCIRef pins the three things that can happen to a
// recorded reference. Fresh index data always wins, which is how a version
// re-published as an archive loses its reference; a version the index no longer
// offers keeps it, without which the addon still using it could not build its
// internal OCIRepository and would be blocked from every change, including its own
// removal.
func TestMergeChartVersionsCarriesOCIRef(t *testing.T) {
	fetched := []repoclient.ChartVersion{
		{Version: semver.MustParse("3.0.0"), OCIRef: "oci://registry.example.com/charts/podinfo:3.0.0"},
		{Version: semver.MustParse("2.0.0")},
	}

	current := []helmv1alpha1.HelmClusterAddonChartVersion{
		{Version: "2.0.0", OCIRef: "oci://registry.example.com/charts/podinfo:2.0.0"},
		{Version: "1.0.0", OCIRef: "oci://registry.example.com/charts/podinfo:1.0.0"},
	}

	inUse := map[string]struct{}{"1.0.0": {}}

	merged := mergeChartVersions(fetched, current, inUse)

	byVersion := map[string]helmv1alpha1.HelmClusterAddonChartVersion{}
	for _, version := range merged {
		byVersion[version.Version] = version
	}

	if got := byVersion["3.0.0"].OCIRef; got != "oci://registry.example.com/charts/podinfo:3.0.0" {
		t.Fatalf("3.0.0 oci ref = %q, want the fetched one", got)
	}
	if got := byVersion["2.0.0"].OCIRef; got != "" {
		t.Fatalf("2.0.0 oci ref = %q, want empty: the index now offers an archive", got)
	}

	retained, ok := byVersion["1.0.0"]
	if !ok {
		t.Fatal("a version still referenced by an addon must be retained")
	}
	if retained.OCIRef != "oci://registry.example.com/charts/podinfo:1.0.0" {
		t.Fatalf("retained oci ref = %q, want the recorded one", retained.OCIRef)
	}
	if retained.UnavailableReason != helmv1alpha1.UnavailableReasonRemovedFromRepository {
		t.Fatalf("retained reason = %q, want %q", retained.UnavailableReason, helmv1alpha1.UnavailableReasonRemovedFromRepository)
	}
}

// TestMergeChartVersionsDoesNotCarryMediaTypeOntoOCIRef pins the invariant the API
// documentation asserts: MediaType stays empty for a version carrying OCIRef. A
// version that now resolves to an OCI artifact must probe its own layer media type
// from scratch even though an addon still references it and a previous pass (back
// when the version was an archive) recorded one: resolveMediaType checks
// version.MediaType != "" before the force-reconcile cache bypass, so a stale
// carried-forward value would use the wrong layer selector and no force reconcile
// could ever correct it.
func TestMergeChartVersionsDoesNotCarryMediaTypeOntoOCIRef(t *testing.T) {
	fetched := []repoclient.ChartVersion{
		{Version: semver.MustParse("6.7.1"), OCIRef: "oci://other-registry.example.com/x/podinfo:6.7.1"},
	}

	current := []helmv1alpha1.HelmClusterAddonChartVersion{
		{Version: "6.7.1", MediaType: "application/tar+gzip"},
	}

	inUse := map[string]struct{}{"6.7.1": {}}

	merged := mergeChartVersions(fetched, current, inUse)

	if len(merged) != 1 {
		t.Fatalf("merged = %+v, want exactly one version", merged)
	}
	if merged[0].OCIRef != "oci://other-registry.example.com/x/podinfo:6.7.1" {
		t.Fatalf("OCIRef = %q, want the fetched one", merged[0].OCIRef)
	}
	if merged[0].MediaType != "" {
		t.Fatalf("MediaType = %q, want empty: a version carrying OCIRef must not carry a stale media type forward", merged[0].MediaType)
	}
}
