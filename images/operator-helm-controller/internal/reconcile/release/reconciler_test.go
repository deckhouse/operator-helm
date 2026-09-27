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

package release

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	helmv2 "github.com/fluxcd/helm-controller/api/v2"
	fluxmeta "github.com/fluxcd/pkg/apis/meta"
	sourcev1 "github.com/fluxcd/source-controller/api/v1"
	"github.com/go-logr/logr/funcr"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/deckhouse/operator-helm/api/naming"
	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
	"github.com/deckhouse/operator-helm/internal/adapter"
	"github.com/deckhouse/operator-helm/internal/chartsource"
	"github.com/deckhouse/operator-helm/internal/claim"
	repoclient "github.com/deckhouse/operator-helm/internal/client/repository"
	"github.com/deckhouse/operator-helm/internal/contract"
	"github.com/deckhouse/operator-helm/internal/index"
	"github.com/deckhouse/operator-helm/internal/internalobj"
	"github.com/deckhouse/operator-helm/internal/namespace"
	"github.com/deckhouse/operator-helm/internal/rbac"
	"github.com/deckhouse/operator-helm/internal/status"
	"github.com/deckhouse/operator-helm/internal/utils"
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("registering client-go scheme: %v", err)
	}
	if err := helmv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("registering helm scheme: %v", err)
	}

	return scheme
}

func newTestReconciler(t *testing.T, objects ...client.Object) (*Reconciler, client.Client) {
	t.Helper()

	scheme := testScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()

	return &Reconciler{Client: c}, c
}

func testAddon() *helmv1alpha1.HelmClusterAddon {
	return &helmv1alpha1.HelmClusterAddon{
		ObjectMeta: metav1.ObjectMeta{Name: "consumer", Generation: 1},
		Spec: helmv1alpha1.HelmClusterAddonSpec{
			Namespace: "app",
			Chart: helmv1alpha1.HelmClusterAddonChartRef{
				HelmClusterAddonRepository: "example",
				HelmClusterAddonChartName:  "podinfo",
				Version:                    "6.7.1",
			},
		},
	}
}

func addonChartFixture(repoName, chartName string, versions ...helmv1alpha1.ChartVersion) *helmv1alpha1.HelmClusterAddonChart {
	return &helmv1alpha1.HelmClusterAddonChart{
		ObjectMeta: metav1.ObjectMeta{
			Name: naming.HelmClusterAddonChartName(repoName, chartName),
		},
		Status: helmv1alpha1.ChartCatalogStatus{Versions: versions},
	}
}

// TestGetHelmClusterAddonChart pins the gate that decides whether an addon has
// enough information to be deployed. For an OCI repository, a catalog entry is
// usable exactly when it carries a media type; for a Helm repository the media
// type is never checked, so an entry is usable as soon as the version is present.
func TestGetHelmClusterAddonChart(t *testing.T) {
	addon := testAddon()

	tests := []struct {
		name string
		// version is the sole entry seeded into the HelmClusterAddonChart's
		// Status.Versions. Its own Version field decides whether the lookup by
		// addon.Spec.Chart.Version ("6.7.1") hits or misses.
		version        helmv1alpha1.ChartVersion
		repoType       chartsource.Kind
		wantErr        bool
		wantErrContain string
	}{
		{
			name: "oci version with a media type passes",
			version: helmv1alpha1.ChartVersion{
				Version:   "6.7.1",
				MediaType: "application/vnd.cncf.helm.chart.content.v1.tar+gzip",
			},
			repoType: chartsource.OCI,
		},
		{
			// Deliberate: the tag disappeared from the repository, but the entry is
			// retained with its media type so the addon keeps reconciling everything
			// else. The real pull failure is reported by the source controller.
			name: "oci version removed from repository but with a media type still passes",
			version: helmv1alpha1.ChartVersion{
				Version:           "6.7.1",
				MediaType:         "application/tar+gzip",
				UnavailableReason: helmv1alpha1.UnavailableReasonRemovedFromRepository,
			},
			repoType: chartsource.OCI,
		},
		{
			name: "oci version stuck resolving is rejected with reason and message",
			version: helmv1alpha1.ChartVersion{
				Version:            "6.7.1",
				UnavailableReason:  helmv1alpha1.UnavailableReasonResolvePending,
				UnavailableMessage: "manifest request failed",
			},
			repoType:       chartsource.OCI,
			wantErr:        true,
			wantErrContain: "ResolvePending: manifest request failed",
		},
		{
			name: "oci version with unsupported media type and no message is rejected with reason alone",
			version: helmv1alpha1.ChartVersion{
				Version:           "6.7.1",
				UnavailableReason: helmv1alpha1.UnavailableReasonUnsupportedMediaType,
			},
			repoType:       chartsource.OCI,
			wantErr:        true,
			wantErrContain: "UnsupportedMediaType",
		},
		{
			// Same empty-media-type entry as above, but a Helm repository's versions
			// never carry a media type: a stricter gate here would break every Helm
			// addon, so the presence check alone must let it through.
			name: "the same empty media type entry passes for a helm repository",
			version: helmv1alpha1.ChartVersion{
				Version:           "6.7.1",
				UnavailableReason: helmv1alpha1.UnavailableReasonUnsupportedMediaType,
			},
			repoType: chartsource.Helm,
		},
		{
			// The repository's URL just switched from oci:// to https://: the
			// catalog entry is still OCI-era (it carries a media type from the last
			// OCI sync), but the Helm gate never reads the media type, so it passes.
			name: "oci-era entry with a media type still passes right after switching to a helm repository",
			version: helmv1alpha1.ChartVersion{
				Version:   "6.7.1",
				MediaType: "application/vnd.cncf.helm.chart.content.v1.tar+gzip",
			},
			repoType: chartsource.Helm,
		},
		{
			// The repository's URL just switched from https:// to oci://, but the
			// first OCI sync has not resolved the tag's media type yet: the entry is
			// still Helm-era (no media type, no reason), so the OCI gate must reject
			// it rather than let an unresolved layer through.
			name: "helm-era entry with no media type is rejected right after switching to an oci repository",
			version: helmv1alpha1.ChartVersion{
				Version: "6.7.1",
			},
			repoType:       chartsource.OCI,
			wantErr:        true,
			wantErrContain: "has not resolved it yet",
		},
		{
			name:           "a version the addon does not reference is rejected",
			version:        helmv1alpha1.ChartVersion{Version: "9.9.9"},
			repoType:       chartsource.OCI,
			wantErr:        true,
			wantErrContain: `does not have version "6.7.1"`,
		},
		{
			// The hybrid case: the version lives in a registry, so its media type is
			// resolved at deploy time and is deliberately absent here. The gate must
			// not read that absence as "unresolved".
			name: "helm repository version published in a registry passes without a media type",
			version: helmv1alpha1.ChartVersion{
				Version: "6.7.1",
				OCIRef:  "oci://registry.example.com/charts/podinfo:6.7.1",
			},
			repoType: chartsource.Helm,
		},
		{
			// Left through, this version would be sent down the helm path and would
			// fail on the same unusable url with an opaque source controller error.
			name: "version with an unusable index reference is rejected",
			version: helmv1alpha1.ChartVersion{
				Version:            "6.7.1",
				UnavailableReason:  helmv1alpha1.UnavailableReasonInvalidChartReference,
				UnavailableMessage: "oci reference \"oci://BAD_HOST//:::\" is not a valid tagged reference",
			},
			repoType:       chartsource.Helm,
			wantErr:        true,
			wantErrContain: "InvalidChartReference",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			chart := addonChartFixture(
				addon.Spec.Chart.HelmClusterAddonRepository, addon.Spec.Chart.HelmClusterAddonChartName, tt.version,
			)
			r, c := newTestReconciler(t, chart)

			gotChart, gotVersion, err := r.getChartVersion(context.Background(), adapter.NewAddonCatalog(c), adapter.NewAddonRepository(helmRepositoryFixture()), adapter.NewAddonRelease(addon), tt.repoType)

			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got version %+v", gotVersion)
				}
				if !strings.Contains(err.Error(), tt.wantErrContain) {
					t.Fatalf("error %q does not contain %q", err.Error(), tt.wantErrContain)
				}
				if gotChart != nil || gotVersion != nil {
					t.Fatalf("expected nil chart and version on error, got chart=%v version=%v", gotChart, gotVersion)
				}

				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if gotChart == nil {
				t.Fatal("expected the chart to be returned")
			}
			if gotVersion == nil {
				t.Fatal("expected the matched version to be returned")
			}
			if gotVersion.Version != tt.version.Version {
				t.Fatalf("returned version = %q, want %q", gotVersion.Version, tt.version.Version)
			}
			if gotVersion.MediaType != tt.version.MediaType {
				t.Fatalf("returned version media type = %q, want %q", gotVersion.MediaType, tt.version.MediaType)
			}
		})
	}
}

// TestResolveSourceTellsAMissingVersionApartFromAnUnsyncedCatalog pins the
// difference the release's status turns on. A catalog with no versions in it says
// nothing about the version asked for — the repository controller has not written it
// yet — while a populated catalog without that version is the repository's full
// answer, and the spec names something it does not offer.
func TestResolveSourceTellsAMissingVersionApartFromAnUnsyncedCatalog(t *testing.T) {
	addon := testAddon()

	tests := []struct {
		name         string
		versions     []helmv1alpha1.ChartVersion
		noCatalog    bool
		synced       bool
		staleSync    bool
		wantReason   string
		wantBadInput bool
		// wantRetried is whether the failure goes to the work queue. The three cases
		// differ in what brings the release back: the two early ones are enqueued by
		// the catalog watches the moment the repository controller writes, so handing
		// them to the rate limiter as well would re-run a pass whose input has not
		// moved and log a controller error for a state that is merely early.
		wantRetried bool
	}{
		{
			name:         "before the first sync an absent catalog object is still progressing",
			noCatalog:    true,
			synced:       false,
			wantReason:   helmv1alpha1.ReasonChartFetchFailed,
			wantBadInput: false,
			wantRetried:  false,
		},
		{
			name:         "before the first sync an empty catalog is still progressing",
			versions:     nil,
			synced:       false,
			wantReason:   helmv1alpha1.ReasonChartFetchFailed,
			wantBadInput: false,
			wantRetried:  false,
		},
		{
			// The repository answered in full and offers no chart under that name: a
			// typo in the chart name, which used to look exactly like a slow sync.
			name:         "after a sync an absent catalog object is a fault of the spec",
			noCatalog:    true,
			synced:       true,
			wantReason:   helmv1alpha1.ReasonChartVersionNotFound,
			wantBadInput: true,
			wantRetried:  false,
		},
		{
			// A chart every one of whose entries the index marks removed, or publishes
			// under a version this operator cannot address, is recorded with no
			// versions and stays that way until the repository changes.
			name:         "after a sync an empty catalog is durable, not transient",
			versions:     nil,
			synced:       true,
			wantReason:   helmv1alpha1.ReasonChartVersionNotFound,
			wantBadInput: true,
			wantRetried:  false,
		},
		{
			name:         "a populated catalog without the version is a fault of the spec",
			versions:     []helmv1alpha1.ChartVersion{{Version: "9.9.9"}},
			synced:       true,
			wantReason:   helmv1alpha1.ReasonChartVersionNotFound,
			wantBadInput: true,
			wantRetried:  false,
		},
		{
			// The repository has a catalog, but of the spec it carried before the url
			// was edited. Reading a chart's absence from it as final would answer
			// about a repository the release no longer points at.
			name:         "a catalog from before a spec change is not the repository's answer",
			noCatalog:    true,
			staleSync:    true,
			wantReason:   helmv1alpha1.ReasonChartFetchFailed,
			wantBadInput: false,
			wantRetried:  false,
		},
		{
			// A version the repository lists without having reached a verdict on how
			// it is published. It may well become deployable, and the write that says
			// so is to the catalog object the release already watches.
			name: "a version still being resolved is progress, not bad input",
			versions: []helmv1alpha1.ChartVersion{{
				Version:            "6.7.1",
				UnavailableReason:  helmv1alpha1.UnavailableReasonResolvePending,
				UnavailableMessage: "the manifest request failed",
			}},
			synced:       true,
			wantReason:   helmv1alpha1.ReasonChartFetchFailed,
			wantBadInput: false,
			wantRetried:  false,
		},
		{
			// The version is listed, and the repository published it at a reference
			// that cannot be addressed. Only a republish changes that, and the catalog
			// watches deliver it; the work queue would spin on an unchanged input.
			name: "a version the repository published unusably is not retried either",
			versions: []helmv1alpha1.ChartVersion{{
				Version:            "6.7.1",
				UnavailableReason:  helmv1alpha1.UnavailableReasonInvalidChartReference,
				UnavailableMessage: "the url does not address a chart",
			}},
			synced:       true,
			wantReason:   helmv1alpha1.ReasonChartVersionNotFound,
			wantBadInput: true,
			wantRetried:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			objects := []client.Object{addonChartFixture(
				addon.Spec.Chart.HelmClusterAddonRepository, addon.Spec.Chart.HelmClusterAddonChartName, tt.versions...,
			)}
			if tt.noCatalog {
				objects = nil
			}

			r, c := newTestReconciler(t, objects...)

			repo := helmRepositoryFixture()
			if tt.synced {
				repo = syncedRepositoryFixture()
			}
			if tt.staleSync {
				repo = syncedRepositoryFixture()
				repo.Generation++
			}

			_, _, failure := r.resolveSource(
				context.Background(),
				adapter.NewAddonRelease(addon),
				adapter.NewAddonRepository(repo),
				adapter.NewAddonCatalog(c),
				chartsource.Helm,
			)

			if failure == nil {
				t.Fatal("expected the version resolution to fail")
			}
			if failure.Reason != tt.wantReason {
				t.Fatalf("reason = %q, want %q", failure.Reason, tt.wantReason)
			}
			if failure.BadInput != tt.wantBadInput {
				t.Fatalf("BadInput = %v, want %v", failure.BadInput, tt.wantBadInput)
			}
			if failure.Err == nil {
				t.Fatal("the cause must be carried out, it is the only place it is written down")
			}
			if retried := retryErr(*failure) != nil; retried != tt.wantRetried {
				t.Fatalf("handed to the work queue = %v, want %v", retried, tt.wantRetried)
			}
		})
	}
}

func TestGetHelmClusterAddonChartMissingChart(t *testing.T) {
	addon := testAddon()
	r, c := newTestReconciler(t)

	gotChart, gotVersion, err := r.getChartVersion(context.Background(), adapter.NewAddonCatalog(c), adapter.NewAddonRepository(helmRepositoryFixture()), adapter.NewAddonRelease(addon), chartsource.OCI)
	if err == nil {
		t.Fatalf("expected an error when the addon chart does not exist, got version %+v", gotVersion)
	}
	if gotChart != nil || gotVersion != nil {
		t.Fatalf("expected nil chart and version on error, got chart=%v version=%v", gotChart, gotVersion)
	}
}

type stubChartResolver struct {
	mediaType string
	err       error
}

func (r *stubChartResolver) ResolveChartArtifact(_ context.Context, _ string, _ *repoclient.RepoConfig) (string, error) {
	return r.mediaType, r.err
}

func helmRepositoryFixture() *helmv1alpha1.HelmClusterAddonRepository {
	return &helmv1alpha1.HelmClusterAddonRepository{
		ObjectMeta: metav1.ObjectMeta{Name: "example", Generation: 1},
		Spec:       helmv1alpha1.RepositorySpec{URL: "https://charts.example.invalid/stable"},
	}
}

// syncedRepositoryFixture is the same repository after it has finished a sync at
// least once. That is what makes its catalog a complete answer rather than a
// half-built one, and several verdicts turn on the difference.
func syncedRepositoryFixture() *helmv1alpha1.HelmClusterAddonRepository {
	repo := helmRepositoryFixture()
	repo.Status.LastSuccessfulSyncTime = &metav1.Time{Time: time.Now().UTC()}
	// The sync described the spec the repository carries now; a lagging
	// observedGeneration would mean the catalog belongs to the previous one.
	repo.Status.ObservedGeneration = repo.Generation

	return repo
}

func newForceTestReconciler(
	t *testing.T,
	interceptors interceptor.Funcs,
	objects ...client.Object,
) (*Reconciler, client.Client) {
	t.Helper()

	return newFullReconciler(t, nil, interceptors, objects...)
}

// newFullReconciler builds a reconciler with the full service set, so a test can
// drive a complete pass rather than a single helper. resolver is handed to the OCI
// service; nil selects the real one, which tests that never reach the hybrid path can
// use safely.
func newFullReconciler(
	t *testing.T,
	resolver repoclient.ChartResolverInterface,
	interceptors interceptor.Funcs,
	objects ...client.Object,
) (*Reconciler, client.Client) {
	t.Helper()

	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		clientgoscheme.AddToScheme,
		helmv1alpha1.AddToScheme,
		sourcev1.AddToScheme,
		helmv2.AddToScheme,
	} {
		if err := add(scheme); err != nil {
			t.Fatalf("registering scheme: %v", err)
		}
	}

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objects...).
		WithStatusSubresource(&helmv1alpha1.HelmClusterAddon{}).
		WithInterceptorFuncs(interceptors).
		Build()

	return New(c, Deps{
		NewRelease:      adapter.EmptyAddonRelease,
		Repositories:    adapter.NewAddonRepositoryResolver(c),
		Charts:          internalobj.NewCharts(c, helmv1alpha1.TargetNamespace),
		OCIRepositories: internalobj.NewOCIRepositories(c, helmv1alpha1.TargetNamespace, resolver),
		Releases:        internalobj.NewReleases(c, helmv1alpha1.TargetNamespace),
		Maintenance:     internalobj.NewMaintenance(c, helmv1alpha1.TargetNamespace),
		Claims:          claim.NewClaims(c, c, helmv1alpha1.TargetNamespace),
		Namespaces:      namespace.NewNamespaces(c, c),
		Identity:        NoIdentity{},
		Status:          status.NewManager(c),
	}), c
}

// newApplicationFullReconciler is the sibling of newFullReconciler for the
// application family: the same services, wired to the namespaced adapters. It is
// what lets a test prove that one reconciler serves both families. identity is the
// identity the release is applied with; nil selects the real one over the same fake
// client.
func newApplicationFullReconciler(
	t *testing.T,
	identity Identity,
	objects ...client.Object,
) (*Reconciler, client.Client) {
	t.Helper()

	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		clientgoscheme.AddToScheme,
		helmv1alpha1.AddToScheme,
		sourcev1.AddToScheme,
		helmv2.AddToScheme,
	} {
		if err := add(scheme); err != nil {
			t.Fatalf("registering scheme: %v", err)
		}
	}

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objects...).
		WithStatusSubresource(&helmv1alpha1.HelmApplication{}).
		WithIndex(&helmv1alpha1.HelmApplication{}, index.ApplicationRepository, index.ApplicationRepositoryIndexer).
		WithIndex(&helmv1alpha1.HelmApplication{}, index.ApplicationChart, index.ApplicationChartIndexer).
		Build()

	if identity == nil {
		identity = rbac.NewIdentity(c, helmv1alpha1.TargetNamespace)
	}

	return New(c, Deps{
		NewRelease:      adapter.EmptyApplicationRelease,
		Repositories:    adapter.NewApplicationRepositoryResolver(c),
		Charts:          internalobj.NewCharts(c, helmv1alpha1.TargetNamespace),
		OCIRepositories: internalobj.NewOCIRepositories(c, helmv1alpha1.TargetNamespace, nil),
		Releases:        internalobj.NewReleases(c, helmv1alpha1.TargetNamespace),
		Maintenance:     internalobj.NewMaintenance(c, helmv1alpha1.TargetNamespace),
		Claims:          NoClaims{},
		Namespaces:      ExistingTargetNamespace{},
		Identity:        identity,
		Status:          status.NewManager(c),
	}), c
}

func testApplication() *helmv1alpha1.HelmApplication {
	return &helmv1alpha1.HelmApplication{
		ObjectMeta: metav1.ObjectMeta{Name: "my-app", Namespace: "team-a", Generation: 1},
		Spec: helmv1alpha1.HelmApplicationSpec{
			Chart: helmv1alpha1.HelmApplicationChartRef{
				Repository: "stable",
				Name:       "podinfo",
				Version:    "6.7.1",
			},
		},
	}
}

func applicationFixtures() []client.Object {
	return []client.Object{
		&helmv1alpha1.HelmApplicationRepository{
			ObjectMeta: metav1.ObjectMeta{Name: "stable", Namespace: "team-a", Generation: 1},
			Spec:       helmv1alpha1.RepositorySpec{URL: "https://charts.example.invalid/stable"},
		},
		&helmv1alpha1.HelmApplicationChart{
			ObjectMeta: metav1.ObjectMeta{
				Name:      naming.ApplicationChartName("stable", "podinfo"),
				Namespace: "team-a",
				Labels: map[string]string{
					helmv1alpha1.LabelRepositoryName: "stable",
					helmv1alpha1.LabelChartName:      "podinfo",
				},
			},
			Status: helmv1alpha1.ChartCatalogStatus{
				Versions: []helmv1alpha1.ChartVersion{{Version: "6.7.1"}},
			},
		},
	}
}

func reconcileApplication(t *testing.T, r *Reconciler, app *helmv1alpha1.HelmApplication) {
	t.Helper()

	if _, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Namespace: app.Namespace, Name: app.Name},
	}); err != nil {
		t.Fatalf("Reconcile returned %v", err)
	}
}

// markInternalChartReady stands in for source-controller: the internal
// HelmChart only reports an artifact once that controller has pulled it, and the
// release stage is reached only when it has.
func markInternalChartReady(t *testing.T, c client.Client, name string) {
	t.Helper()

	chart := &sourcev1.HelmChart{}
	key := client.ObjectKey{Name: name, Namespace: helmv1alpha1.TargetNamespace}
	if err := c.Get(context.Background(), key, chart); err != nil {
		t.Fatalf("the internal helm chart must exist before it can report an artifact: %v", err)
	}

	chart.Status.Artifact = &fluxmeta.Artifact{Revision: "6.7.1"}
	chart.Status.Conditions = []metav1.Condition{{
		Type:               "Ready",
		Status:             metav1.ConditionTrue,
		Reason:             "Succeeded",
		ObservedGeneration: chart.Generation,
		LastTransitionTime: metav1.Now(),
	}}
	// A plain Update, not Status().Update: the fake client is not told to give
	// HelmChart a status subresource, and the controller never writes that status
	// itself, so there is nothing for a subresource to protect here.
	if err := c.Update(context.Background(), chart); err != nil {
		t.Fatalf("updating internal helm chart status: %v", err)
	}
}

// markInternalReleaseDeployed stands in for helm-controller: the internal
// HelmRelease reports the revision it installed only once that controller has run.
func markInternalReleaseDeployed(t *testing.T, c client.Client, name, version string) {
	t.Helper()

	release := &helmv2.HelmRelease{}
	key := client.ObjectKey{Name: name, Namespace: helmv1alpha1.TargetNamespace}
	if err := c.Get(context.Background(), key, release); err != nil {
		t.Fatalf("the internal release must exist before it can report a revision: %v", err)
	}

	release.Status.History = helmv2.Snapshots{{Status: "deployed", ChartVersion: version}}
	release.Status.Conditions = []metav1.Condition{{
		Type:               "Ready",
		Status:             metav1.ConditionTrue,
		Reason:             "InstallSucceeded",
		ObservedGeneration: release.Generation,
		LastTransitionTime: metav1.Now(),
	}}
	if err := c.Update(context.Background(), release); err != nil {
		t.Fatalf("updating internal helm release status: %v", err)
	}
}

// TestReconcileReportsProgressUntilTheReleaseSettles walks the progress condition
// through one install. It is raised while an internal object still has work to do,
// carries that object's own verdict rather than a fixed message, and is taken away
// by the pass that finds the release settled — without which kstatus would read a
// healthy release as one that never finishes. The passes in between are driven by
// the watches on the internal objects, which is what a status write on either of
// them produces in the real controller.
func TestReconcileReportsProgressUntilTheReleaseSettles(t *testing.T) {
	app := testApplication()

	r, c := newApplicationFullReconciler(t, nil, append(applicationFixtures(), app)...)

	names := adapter.NewApplicationRelease(app).InternalNames()
	key := types.NamespacedName{Namespace: app.Namespace, Name: app.Name}

	progressOf := func(t *testing.T) *metav1.Condition {
		t.Helper()

		settled := &helmv1alpha1.HelmApplication{}
		if err := c.Get(context.Background(), key, settled); err != nil {
			t.Fatalf("getting application: %v", err)
		}

		return apimeta.FindStatusCondition(settled.Status.Conditions, helmv1alpha1.ConditionTypeReconciling)
	}

	reconcileApplication(t, r, app)

	progress := progressOf(t)
	if progress == nil || progress.Status != metav1.ConditionTrue {
		t.Fatalf("Reconciling = %+v, want True while the chart is being pulled", progress)
	}
	if progress.Reason != helmv1alpha1.ReasonReconciling {
		t.Fatalf("Reconciling reason = %q, want %q", progress.Reason, helmv1alpha1.ReasonReconciling)
	}

	markInternalChartReady(t, c, names.HelmChart)
	reconcileApplication(t, r, app)

	if progress := progressOf(t); progress == nil || progress.Status != metav1.ConditionTrue {
		t.Fatalf("Reconciling = %+v, want True while the release is rolling out", progress)
	}

	markInternalReleaseDeployed(t, c, names.HelmRelease, app.Spec.Chart.Version)
	reconcileApplication(t, r, app)

	if progress := progressOf(t); progress != nil {
		t.Fatalf("Reconciling = %+v, want it gone once the release settled", progress)
	}
}

// TestReconcileStallsWhenTheInternalReleaseGivesUp wires the two halves of the
// verdict together on the path a test cluster actually took: bad values leave the
// internal release Stalled once its remediation attempts are spent. The services
// layer has to carry that out of the object's conditions and the evaluation has to
// act on it; each is covered on its own, and this is what proves they meet.
func TestReconcileStallsWhenTheInternalReleaseGivesUp(t *testing.T) {
	const cause = "Helm upgrade failed: .spec.replicas: expected numeric (int or float), got string"

	app := testApplication()

	r, c := newApplicationFullReconciler(t, nil, append(applicationFixtures(), app)...)

	names := adapter.NewApplicationRelease(app).InternalNames()
	key := types.NamespacedName{Namespace: app.Namespace, Name: app.Name}

	reconcileApplication(t, r, app)
	markInternalChartReady(t, c, names.HelmChart)
	reconcileApplication(t, r, app)

	release := &helmv2.HelmRelease{}
	releaseKey := client.ObjectKey{Name: names.HelmRelease, Namespace: helmv1alpha1.TargetNamespace}
	if err := c.Get(context.Background(), releaseKey, release); err != nil {
		t.Fatalf("the internal release must exist before it can give up: %v", err)
	}
	release.Status.Conditions = []metav1.Condition{
		{
			Type:               helmv1alpha1.ConditionTypeStalled,
			Status:             metav1.ConditionTrue,
			Reason:             helmv1alpha1.ReasonRetriesExceeded,
			Message:            "Failed to upgrade after 1 attempt(s)",
			ObservedGeneration: release.Generation,
			LastTransitionTime: metav1.Now(),
		},
		{
			Type:               "Released",
			Status:             metav1.ConditionFalse,
			Reason:             "UpgradeFailed",
			Message:            cause,
			ObservedGeneration: release.Generation,
			LastTransitionTime: metav1.Now(),
		},
	}
	if err := c.Update(context.Background(), release); err != nil {
		t.Fatalf("updating internal helm release status: %v", err)
	}

	reconcileApplication(t, r, app)

	settled := &helmv1alpha1.HelmApplication{}
	if err := c.Get(context.Background(), key, settled); err != nil {
		t.Fatalf("getting application: %v", err)
	}

	stalled := apimeta.FindStatusCondition(settled.Status.Conditions, helmv1alpha1.ConditionTypeStalled)
	if stalled == nil || stalled.Status != metav1.ConditionTrue {
		t.Fatalf("Stalled = %+v, want True once the internal release gave up", stalled)
	}
	if stalled.Reason != helmv1alpha1.ReasonReleaseFailed || stalled.Message != cause {
		t.Fatalf("Stalled = %s/%q, want the fault the internal release named", stalled.Reason, stalled.Message)
	}

	if cond := apimeta.FindStatusCondition(settled.Status.Conditions, helmv1alpha1.ConditionTypeReconciling); cond != nil {
		t.Fatalf("Reconciling = %+v, want no retry reported that is not coming", cond)
	}
}

// TestReconcileApplicationAppliesTheChartAsItsOwnIdentity is the central claim of
// the namespaced family: the release is created in the operator namespace, but it
// is applied as an account of the application's own making, and its storage lives
// in the application's namespace — so an application can never reach outside it.
func TestReconcileApplicationAppliesTheChartAsItsOwnIdentity(t *testing.T) {
	app := testApplication()

	r, c := newApplicationFullReconciler(t, nil, append(applicationFixtures(), app)...)

	names := adapter.NewApplicationRelease(app).InternalNames()

	// The first pass adds the finalizer and creates the internal chart; the second
	// one reaches the release, once that chart reports an artifact.
	reconcileApplication(t, r, app)
	markInternalChartReady(t, c, names.HelmChart)
	reconcileApplication(t, r, app)

	account := &corev1.ServiceAccount{}
	accountKey := client.ObjectKey{Name: names.ServiceAccount, Namespace: helmv1alpha1.TargetNamespace}
	if err := c.Get(context.Background(), accountKey, account); err != nil {
		t.Fatalf("the identity must be created next to the release: %v", err)
	}
	if account.AutomountServiceAccountToken == nil || *account.AutomountServiceAccountToken {
		t.Fatal("the account is only a subject name: no token must be mounted for it")
	}

	release := &helmv2.HelmRelease{}
	releaseKey := client.ObjectKey{Name: names.HelmRelease, Namespace: helmv1alpha1.TargetNamespace}
	if err := c.Get(context.Background(), releaseKey, release); err != nil {
		t.Fatalf("the internal release was not created: %v", err)
	}
	if release.Spec.ServiceAccountName != names.ServiceAccount {
		t.Fatalf("serviceAccountName = %q, want %q", release.Spec.ServiceAccountName, names.ServiceAccount)
	}
	if release.Spec.StorageNamespace != "team-a" {
		t.Fatalf("storageNamespace = %q, want the application namespace", release.Spec.StorageNamespace)
	}

	chartKey := client.ObjectKey{Name: names.HelmChart, Namespace: helmv1alpha1.TargetNamespace}
	if err := c.Get(context.Background(), chartKey, &sourcev1.HelmChart{}); err != nil {
		t.Fatalf("the internal chart was not created: %v", err)
	}
}

// TestReconcileApplicationCreatesNothingElseInTheApplicationNamespace is the other
// half of the same claim: everything the controller runs on lives in the operator
// namespace. The application namespace only ever receives the two RBAC objects
// that grant the identity its rights there.
func TestReconcileApplicationCreatesNothingElseInTheApplicationNamespace(t *testing.T) {
	app := testApplication()

	r, c := newApplicationFullReconciler(t, nil, append(applicationFixtures(), app)...)

	names := adapter.NewApplicationRelease(app).InternalNames()

	reconcileApplication(t, r, app)
	markInternalChartReady(t, c, names.HelmChart)
	reconcileApplication(t, r, app)

	inNamespace := client.InNamespace("team-a")

	empty := []struct {
		name string
		list client.ObjectList
	}{
		{"config maps", &corev1.ConfigMapList{}},
		{"secrets", &corev1.SecretList{}},
		{"helm charts", &sourcev1.HelmChartList{}},
		{"helm releases", &helmv2.HelmReleaseList{}},
		{"oci repositories", &sourcev1.OCIRepositoryList{}},
		{"service accounts", &corev1.ServiceAccountList{}},
	}
	for _, tt := range empty {
		if err := c.List(context.Background(), tt.list, inNamespace); err != nil {
			t.Fatalf("listing %s: %v", tt.name, err)
		}
		items, err := apimeta.ExtractList(tt.list)
		if err != nil {
			t.Fatalf("extracting %s: %v", tt.name, err)
		}
		if len(items) != 0 {
			t.Fatalf("%d %s were created in the application namespace, want none: %v", len(items), tt.name, items)
		}
	}

	var roles rbacv1.RoleList
	if err := c.List(context.Background(), &roles, inNamespace); err != nil {
		t.Fatalf("listing roles: %v", err)
	}
	if len(roles.Items) != 1 || roles.Items[0].Name != rbac.ApplicationRoleName {
		t.Fatalf("roles = %+v, want only %s", roles.Items, rbac.ApplicationRoleName)
	}

	var bindings rbacv1.RoleBindingList
	if err := c.List(context.Background(), &bindings, inNamespace); err != nil {
		t.Fatalf("listing role bindings: %v", err)
	}
	if len(bindings.Items) != 1 || bindings.Items[0].Name != names.ServiceAccount {
		t.Fatalf("role bindings = %+v, want only %s", bindings.Items, names.ServiceAccount)
	}
}

type failingIdentity struct{}

func (failingIdentity) Ensure(context.Context, contract.Release) rbac.IdentityOutcome {
	return rbac.IdentityOutcome{
		Err:     errors.New("service account is forbidden"),
		Reason:  helmv1alpha1.ReasonRBACSetupFailed,
		Message: "Failed to set up the release identity",
	}
}

func (failingIdentity) Cleanup(context.Context, contract.Release) error { return nil }

// TestReconcileApplicationReportsRBACSetupFailure pins that the pass stops at the
// identity. A release applied without one would run as helm-controller itself,
// which is exactly the privilege the namespaced family exists to avoid, so the
// failure has to be reported instead of worked around. It must also be returned
// as an error: nothing watches the ServiceAccount/RoleBinding this step manages,
// so the work queue's rate limiter is the only thing that will retry it.
func TestReconcileApplicationReportsRBACSetupFailure(t *testing.T) {
	app := testApplication()

	r, c := newApplicationFullReconciler(t, failingIdentity{}, append(applicationFixtures(), app)...)

	key := types.NamespacedName{Namespace: app.Namespace, Name: app.Name}
	if _, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: key}); err == nil {
		t.Fatal("a failed identity setup must be returned as an error so the work queue retries it")
	}

	settled := &helmv1alpha1.HelmApplication{}
	if err := c.Get(context.Background(), key, settled); err != nil {
		t.Fatalf("getting application: %v", err)
	}

	ready := apimeta.FindStatusCondition(settled.Status.Conditions, helmv1alpha1.ConditionTypeReady)
	if ready == nil {
		t.Fatalf("Ready must be reported, conditions: %v", settled.Status.Conditions)
	}
	if ready.Status != metav1.ConditionFalse || ready.Reason != helmv1alpha1.ReasonRBACSetupFailed {
		t.Fatalf("Ready is %s/%s, want False/%s", ready.Status, ready.Reason, helmv1alpha1.ReasonRBACSetupFailed)
	}

	names := adapter.NewApplicationRelease(app).InternalNames()
	releaseKey := client.ObjectKey{Name: names.HelmRelease, Namespace: helmv1alpha1.TargetNamespace}
	if err := c.Get(context.Background(), releaseKey, &helmv2.HelmRelease{}); err == nil {
		t.Fatal("no release must be created for an application whose identity could not be set up")
	}
}

// intermittentIdentity fails the first call to Ensure and delegates to a real
// Identity afterwards, standing in for a transient failure (an API hiccup, a
// momentary admission rejection) that clears on its own by the next pass. real is
// set after construction, once the reconciler's own client exists.
type intermittentIdentity struct {
	real  Identity
	calls int
}

func (a *intermittentIdentity) Ensure(ctx context.Context, rel contract.Release) rbac.IdentityOutcome {
	a.calls++
	if a.calls == 1 {
		return rbac.IdentityOutcome{
			Err:     errors.New("service account is forbidden"),
			Reason:  helmv1alpha1.ReasonRBACSetupFailed,
			Message: "Failed to set up the release identity",
		}
	}
	return a.real.Ensure(ctx, rel)
}

func (a *intermittentIdentity) Cleanup(ctx context.Context, rel contract.Release) error {
	return a.real.Cleanup(ctx, rel)
}

// TestReconcileApplicationRecoversAfterTransientRBACSetupFailure pins the fix for
// the gap in TestReconcileApplicationReportsRBACSetupFailure: a failed
// Ensure must not just be reported, it must get the application requeued, so
// a transient failure recovers on its own once the cause is gone.
func TestReconcileApplicationRecoversAfterTransientRBACSetupFailure(t *testing.T) {
	app := testApplication()

	flaky := &intermittentIdentity{}
	r, c := newApplicationFullReconciler(t, flaky, append(applicationFixtures(), app)...)
	flaky.real = rbac.NewIdentity(c, helmv1alpha1.TargetNamespace)

	key := types.NamespacedName{Namespace: app.Namespace, Name: app.Name}

	if _, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: key}); err == nil {
		t.Fatal("the first pass must report the transient failure as an error")
	}

	settled := &helmv1alpha1.HelmApplication{}
	if err := c.Get(context.Background(), key, settled); err != nil {
		t.Fatalf("getting application: %v", err)
	}
	ready := apimeta.FindStatusCondition(settled.Status.Conditions, helmv1alpha1.ConditionTypeReady)
	if ready == nil || ready.Status != metav1.ConditionFalse || ready.Reason != helmv1alpha1.ReasonRBACSetupFailed {
		t.Fatalf("Ready = %+v, want False/%s after the first pass", ready, helmv1alpha1.ReasonRBACSetupFailed)
	}

	if _, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: key}); err != nil {
		t.Fatalf("the retried pass must succeed once the identity can be set up: %v", err)
	}

	names := adapter.NewApplicationRelease(app).InternalNames()
	account := &corev1.ServiceAccount{}
	accountKey := client.ObjectKey{Name: names.ServiceAccount, Namespace: helmv1alpha1.TargetNamespace}
	if err := c.Get(context.Background(), accountKey, account); err != nil {
		t.Fatalf("the identity must be created once Ensure stops failing: %v", err)
	}

	if err := c.Get(context.Background(), key, settled); err != nil {
		t.Fatalf("getting application: %v", err)
	}
	if reason := apimeta.FindStatusCondition(settled.Status.Conditions, helmv1alpha1.ConditionTypeReady).Reason; reason == helmv1alpha1.ReasonRBACSetupFailed {
		t.Fatal("Ready must move on from RBACSetupFailed once the retried pass sets up the identity")
	}
}

// TestReconcileStallsOnAnUnreadableRepositoryURL pins the release-side mirror of the
// repository's own configuration verdict. The fault is in the repository, not here,
// and the repository reports it as Stalled too; retrying from this side would only
// rediscover it, so the release says so and waits for the repository's generation to
// move — which is what correcting the url does.
func TestReconcileStallsOnAnUnreadableRepositoryURL(t *testing.T) {
	app := testApplication()
	fixtures := applicationFixtures()
	fixtures[0] = &helmv1alpha1.HelmApplicationRepository{
		ObjectMeta: metav1.ObjectMeta{Name: "stable", Namespace: "team-a", Generation: 1},
		Spec:       helmv1alpha1.RepositorySpec{URL: "ftp://charts.example.invalid/stable"},
	}

	r, c := newApplicationFullReconciler(t, nil, append(fixtures, app)...)

	key := types.NamespacedName{Namespace: app.Namespace, Name: app.Name}
	result, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: key})
	if err != nil {
		t.Fatalf("a terminal failure must not be handed to the work queue: %v", err)
	}
	if result.RequeueAfter != 0 {
		t.Fatalf("RequeueAfter = %v, want none", result.RequeueAfter)
	}

	settled := &helmv1alpha1.HelmApplication{}
	if err := c.Get(context.Background(), key, settled); err != nil {
		t.Fatalf("getting application: %v", err)
	}

	stalled := apimeta.FindStatusCondition(settled.Status.Conditions, helmv1alpha1.ConditionTypeStalled)
	if stalled == nil {
		t.Fatalf("Stalled must be reported, conditions: %v", settled.Status.Conditions)
	}
	if stalled.Status != metav1.ConditionTrue || stalled.Reason != helmv1alpha1.ReasonUnsupportedRepositoryType {
		t.Fatalf("Stalled is %s/%s, want True/%s",
			stalled.Status, stalled.Reason, helmv1alpha1.ReasonUnsupportedRepositoryType)
	}
}

// TestReconcileApplicationStallsOnAForeignRole pins the end of the terminal path an
// application reaches when the namespace already holds a Role under the name the
// identity needs. Nothing observes such an object — the informer behind the watch on
// the kind selects on the very label it lacks — so the pass must say so on the status
// rather than come back through the work queue and rediscover it.
func TestReconcileApplicationStallsOnAForeignRole(t *testing.T) {
	app := testApplication()
	rules := []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"configmaps"}, Verbs: []string{"get"}}}
	foreign := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: rbac.ApplicationRoleName, Namespace: app.Namespace},
		Rules:      rules,
	}

	r, c := newApplicationFullReconciler(t, nil, append(applicationFixtures(), app, foreign)...)

	key := types.NamespacedName{Namespace: app.Namespace, Name: app.Name}
	result, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: key})
	if err != nil {
		t.Fatalf("a terminal failure must not be handed to the work queue: %v", err)
	}
	if result.RequeueAfter != 0 {
		t.Fatalf("RequeueAfter = %v, want none", result.RequeueAfter)
	}

	settled := &helmv1alpha1.HelmApplication{}
	if err := c.Get(context.Background(), key, settled); err != nil {
		t.Fatalf("getting application: %v", err)
	}

	stalled := apimeta.FindStatusCondition(settled.Status.Conditions, helmv1alpha1.ConditionTypeStalled)
	if stalled == nil {
		t.Fatalf("Stalled must be reported, conditions: %v", settled.Status.Conditions)
	}
	if stalled.Status != metav1.ConditionTrue || stalled.Reason != helmv1alpha1.ReasonForeignRBACObject {
		t.Fatalf("Stalled is %s/%s, want True/%s", stalled.Status, stalled.Reason, helmv1alpha1.ReasonForeignRBACObject)
	}

	ready := apimeta.FindStatusCondition(settled.Status.Conditions, helmv1alpha1.ConditionTypeReady)
	if ready == nil || ready.Status != metav1.ConditionFalse || ready.Reason != helmv1alpha1.ReasonForeignRBACObject {
		t.Fatalf("Ready = %+v, want False/%s", ready, helmv1alpha1.ReasonForeignRBACObject)
	}

	stored := &rbacv1.Role{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(foreign), stored); err != nil {
		t.Fatalf("the foreign role must survive: %v", err)
	}
	if !reflect.DeepEqual(stored.Rules, rules) {
		t.Fatalf("rules = %+v, want them untouched", stored.Rules)
	}

	names := adapter.NewApplicationRelease(app).InternalNames()
	releaseKey := client.ObjectKey{Name: names.HelmRelease, Namespace: helmv1alpha1.TargetNamespace}
	if err := c.Get(context.Background(), releaseKey, &helmv2.HelmRelease{}); err == nil {
		t.Fatal("no release must be created for an application whose identity could not be built")
	}
}

type accessCleanupFails struct{}

func (accessCleanupFails) Ensure(context.Context, contract.Release) rbac.IdentityOutcome {
	return rbac.IdentityOutcome{}
}

func (accessCleanupFails) Cleanup(context.Context, contract.Release) error {
	return errors.New("role binding deletion forbidden")
}

// TestReconcileDeleteReportsRBACCleanupFailure pins that a failed Cleanup
// leaves the status saying so. By the time this step runs the internal release and
// sources are already gone, so nothing else on the object would otherwise explain
// why the finalizer is still there.
func TestReconcileDeleteReportsRBACCleanupFailure(t *testing.T) {
	now := metav1.Now()
	app := &helmv1alpha1.HelmApplication{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "my-app",
			Namespace:         "team-a",
			Generation:        1,
			Finalizers:        []string{helmv1alpha1.FinalizerName},
			DeletionTimestamp: &now,
		},
		Spec: testApplication().Spec,
	}

	r, c := newApplicationFullReconciler(t, accessCleanupFails{}, app)

	key := types.NamespacedName{Namespace: app.Namespace, Name: app.Name}
	if _, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: key}); err == nil {
		t.Fatal("a failed identity cleanup must be returned as an error so the finalizer's retry fires")
	}

	settled := &helmv1alpha1.HelmApplication{}
	if err := c.Get(context.Background(), key, settled); err != nil {
		t.Fatalf("getting application: %v", err)
	}

	if !controllerutil.ContainsFinalizer(settled, helmv1alpha1.FinalizerName) {
		t.Fatal("the finalizer must stay until the identity is actually cleaned up")
	}

	ready := apimeta.FindStatusCondition(settled.Status.Conditions, helmv1alpha1.ConditionTypeReady)
	if ready == nil {
		t.Fatalf("Ready must be reported, conditions: %v", settled.Status.Conditions)
	}
	if ready.Status != metav1.ConditionFalse || ready.Reason != helmv1alpha1.ReasonFailed {
		t.Fatalf("Ready is %s/%s, want False/%s", ready.Status, ready.Reason, helmv1alpha1.ReasonFailed)
	}
}

func ociRepositoryFixture() *helmv1alpha1.HelmClusterAddonRepository {
	return &helmv1alpha1.HelmClusterAddonRepository{
		ObjectMeta: metav1.ObjectMeta{Name: "example", Generation: 1},
		Spec:       helmv1alpha1.RepositorySpec{URL: "oci://ghcr.io/example/podinfo"},
	}
}

func forceTestFixtures() []client.Object {
	return []client.Object{
		ociRepositoryFixture(),
		addonChartFixture("example", "podinfo", helmv1alpha1.ChartVersion{
			Version:   "6.7.1",
			MediaType: "application/vnd.cncf.helm.chart.content.v1.tar+gzip",
		}),
	}
}

func reconcileAddon(t *testing.T, r *Reconciler, name string) {
	t.Helper()

	if _, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: name},
	}); err != nil {
		t.Fatalf("Reconcile returned %v", err)
	}
}

// TestReconcileHybridVersionUsesInternalOCIRepository is the end-to-end shape of the
// feature: the repository is a classic helm one, and only the index entry of the
// version points at a registry. The addon must be served by an internal
// OCIRepository addressed by that entry, and no internal HelmChart must be created.
func TestReconcileHybridVersionUsesInternalOCIRepository(t *testing.T) {
	addon := testAddon()
	resolver := &stubChartResolver{mediaType: "application/vnd.cncf.helm.chart.content.v1.tar+gzip"}

	r, c := newFullReconciler(t, resolver, interceptor.Funcs{},
		addon,
		helmRepositoryFixture(),
		addonChartFixture("example", "podinfo", helmv1alpha1.ChartVersion{
			Version: "6.7.1",
			OCIRef:  "oci://registry.example.com/charts/podinfo:6.7.1",
		}),
	)

	reconcileAddon(t, r, addon.Name)

	ociRepo := &sourcev1.OCIRepository{}
	ociKey := client.ObjectKey{
		Name:      utils.GetInternalOCIRepositoryName(addon.Name),
		Namespace: helmv1alpha1.TargetNamespace,
	}
	if err := c.Get(context.Background(), ociKey, ociRepo); err != nil {
		t.Fatalf("a version published in a registry must be served by an internal oci repository: %v", err)
	}
	if ociRepo.Spec.URL != "oci://registry.example.com/charts/podinfo" {
		t.Fatalf("url = %q, want the address from the index entry", ociRepo.Spec.URL)
	}
	if ociRepo.Spec.Reference == nil || ociRepo.Spec.Reference.Tag != "6.7.1" {
		t.Fatalf("reference = %+v, want tag 6.7.1", ociRepo.Spec.Reference)
	}
	if ociRepo.Spec.LayerSelector == nil || ociRepo.Spec.LayerSelector.MediaType != resolver.mediaType {
		t.Fatalf("layer selector = %+v, want the examined media type", ociRepo.Spec.LayerSelector)
	}

	chart := &sourcev1.HelmChart{}
	chartKey := client.ObjectKey{
		Name:      utils.GetInternalHelmChartName(addon.Name),
		Namespace: helmv1alpha1.TargetNamespace,
	}
	if err := c.Get(context.Background(), chartKey, chart); err == nil {
		t.Fatal("no internal helm chart must be created for a version published in a registry")
	}
}

// TestReconcileArchiveVersionOfHelmRepositoryStaysOnTheHelmPath is the complement:
// the same repository, a version without an index reference, and nothing about the
// hybrid path must engage.
func TestReconcileArchiveVersionOfHelmRepositoryStaysOnTheHelmPath(t *testing.T) {
	addon := testAddon()

	r, c := newFullReconciler(t, &stubChartResolver{}, interceptor.Funcs{},
		addon,
		helmRepositoryFixture(),
		addonChartFixture("example", "podinfo", helmv1alpha1.ChartVersion{
			Version: "6.7.1",
		}),
	)

	reconcileAddon(t, r, addon.Name)

	chart := &sourcev1.HelmChart{}
	chartKey := client.ObjectKey{
		Name:      utils.GetInternalHelmChartName(addon.Name),
		Namespace: helmv1alpha1.TargetNamespace,
	}
	if err := c.Get(context.Background(), chartKey, chart); err != nil {
		t.Fatalf("an archive version must be served by an internal helm chart: %v", err)
	}

	ociRepo := &sourcev1.OCIRepository{}
	ociKey := client.ObjectKey{
		Name:      utils.GetInternalOCIRepositoryName(addon.Name),
		Namespace: helmv1alpha1.TargetNamespace,
	}
	if err := c.Get(context.Background(), ociKey, ociRepo); err == nil {
		t.Fatal("no internal oci repository must be created for an archive version")
	}
}

// TestReconcileVersionMovedOutOfRegistrySupersedesTheOCIRepository is the mirror flip:
// the index re-published a version the addon is already running as an archive from an
// internal OCIRepository, either because the user repointed the repository or because
// the index re-published it out of the registry. The superseded internal OCIRepository
// is removed even though the new source has not produced an artifact yet, for the same
// reason as its HelmChart counterpart: a repository retracting a location is a fact
// the addon state has to reflect, and keeping the old source would let the addon keep
// deploying from a place the repository no longer offers.
func TestReconcileVersionMovedOutOfRegistrySupersedesTheOCIRepository(t *testing.T) {
	addon := testAddon()
	addon.Status.LastAppliedChart = &helmv1alpha1.HelmClusterAddonLastAppliedChartRef{
		HelmClusterAddonRepository: "example",
		HelmClusterAddonChartName:  "podinfo",
		Version:                    "6.7.1",
	}

	supersededOCIRepo := &sourcev1.OCIRepository{
		ObjectMeta: metav1.ObjectMeta{
			Name:      utils.GetInternalOCIRepositoryName(addon.Name),
			Namespace: helmv1alpha1.TargetNamespace,
		},
	}

	r, c := newFullReconciler(t, &stubChartResolver{}, interceptor.Funcs{},
		addon,
		helmRepositoryFixture(),
		supersededOCIRepo,
		addonChartFixture("example", "podinfo", helmv1alpha1.ChartVersion{
			Version: "6.7.1",
		}),
	)

	reconcileAddon(t, r, addon.Name)

	ociRepo := &sourcev1.OCIRepository{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(supersededOCIRepo), ociRepo); err == nil {
		t.Error("the superseded internal oci repository must be removed")
	}

	chart := &sourcev1.HelmChart{}
	chartKey := client.ObjectKey{
		Name:      utils.GetInternalHelmChartName(addon.Name),
		Namespace: helmv1alpha1.TargetNamespace,
	}
	if err := c.Get(context.Background(), chartKey, chart); err != nil {
		t.Fatalf("the new source must be created in the same pass: %v", err)
	}
}

// TestReconcileVersionMovedIntoRegistrySupersedesTheHelmChart is the flip: the index
// re-published a version the addon is already running as an OCI artifact. The
// superseded internal HelmChart is removed even though the new source has not
// produced an artifact yet — a repository retracting a location is a fact the addon
// state has to reflect, and keeping the old source would let the addon keep deploying
// from a place the repository no longer offers. The running release is not torn down
// by that: helm-controller does not uninstall a release because its source is gone.
func TestReconcileVersionMovedIntoRegistrySupersedesTheHelmChart(t *testing.T) {
	addon := testAddon()
	addon.Status.LastAppliedChart = &helmv1alpha1.HelmClusterAddonLastAppliedChartRef{
		HelmClusterAddonRepository: "example",
		HelmClusterAddonChartName:  "podinfo",
		Version:                    "6.7.1",
	}

	supersededChart := &sourcev1.HelmChart{
		ObjectMeta: metav1.ObjectMeta{
			Name:      utils.GetInternalHelmChartName(addon.Name),
			Namespace: helmv1alpha1.TargetNamespace,
		},
	}

	r, c := newFullReconciler(t, &stubChartResolver{mediaType: "application/tar+gzip"}, interceptor.Funcs{},
		addon,
		helmRepositoryFixture(),
		supersededChart,
		addonChartFixture("example", "podinfo", helmv1alpha1.ChartVersion{
			Version: "6.7.1",
			OCIRef:  "oci://registry.example.com/charts/podinfo:6.7.1",
		}),
	)

	reconcileAddon(t, r, addon.Name)

	chart := &sourcev1.HelmChart{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(supersededChart), chart); err == nil {
		t.Error("the superseded internal helm chart must be removed")
	}

	ociRepo := &sourcev1.OCIRepository{}
	ociKey := client.ObjectKey{
		Name:      utils.GetInternalOCIRepositoryName(addon.Name),
		Namespace: helmv1alpha1.TargetNamespace,
	}
	if err := c.Get(context.Background(), ociKey, ociRepo); err != nil {
		t.Fatalf("the new source must be created in the same pass: %v", err)
	}
	if ociRepo.Spec.URL != "oci://registry.example.com/charts/podinfo" {
		t.Fatalf("url = %q, want the address from the index entry", ociRepo.Spec.URL)
	}
}

// TestLogSourceKindFlipIgnoresStaleEntryFromADifferentChartOrRepository pins the
// guard added to logSourceKindFlip: LastAppliedChart carries its own repository/chart
// identity and can lag behind Spec.Chart, so a version string that happens to match
// is not enough on its own — the repository and chart name have to match too, or an
// addon that switched to an unrelated chart reusing the same version string would be
// misreported as its current chart having changed where it is published.
func TestLogSourceKindFlipIgnoresStaleEntryFromADifferentChartOrRepository(t *testing.T) {
	tests := []struct {
		name       string
		last       *helmv1alpha1.HelmClusterAddonLastAppliedChartRef
		wantLogged bool
	}{
		{
			name: "same repository, chart and version is a flip",
			last: &helmv1alpha1.HelmClusterAddonLastAppliedChartRef{
				HelmClusterAddonRepository: "example",
				HelmClusterAddonChartName:  "podinfo",
				Version:                    "6.7.1",
			},
			wantLogged: true,
		},
		{
			// The version string coincides, but it belongs to a different chart's
			// history: the addon was repointed, not flipped.
			name: "same version but a different chart name is not a flip",
			last: &helmv1alpha1.HelmClusterAddonLastAppliedChartRef{
				HelmClusterAddonRepository: "example",
				HelmClusterAddonChartName:  "other-chart",
				Version:                    "6.7.1",
			},
			wantLogged: false,
		},
		{
			// Same reasoning, the other field: the version string coincides, but it
			// belongs to a different repository's history.
			name: "same version but a different repository is not a flip",
			last: &helmv1alpha1.HelmClusterAddonLastAppliedChartRef{
				HelmClusterAddonRepository: "other-repo",
				HelmClusterAddonChartName:  "podinfo",
				Version:                    "6.7.1",
			},
			wantLogged: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			addon := testAddon()
			addon.Status.LastAppliedChart = tt.last

			var logged bool
			logger := funcr.New(func(prefix, args string) {
				logged = true
			}, funcr.Options{})
			ctx := log.IntoContext(context.Background(), logger)

			r := &Reconciler{}
			r.logSourceKindFlip(ctx, adapter.NewAddonRelease(addon), chartsource.OCI, true)

			if logged != tt.wantLogged {
				t.Fatalf("logged = %v, want %v", logged, tt.wantLogged)
			}
		})
	}
}

// TestReconcileForcedAddonReportsProgressBeforeWorking pins that Reconciling is
// published before the internal source is touched. The user annotated the addon a
// moment ago and is watching it; a condition written only after the release has
// been reconciled would report progress that is already over.
func TestReconcileForcedAddonReportsProgressBeforeWorking(t *testing.T) {
	addon := testAddon()
	addon.Annotations = map[string]string{helmv1alpha1.AnnotationForceReconcile: "2026-01-01T00:00:00Z"}

	var inFlight *metav1.Condition
	var c client.Client

	// The internal source is reconciled with CreateOrPatch, so the first write to
	// it is a Create on a fresh addon and a Patch on an existing one; hook both so
	// the test does not depend on which one this fixture takes.
	captureAddonStatus := func(ctx context.Context) {
		if inFlight != nil {
			return
		}

		observed := &helmv1alpha1.HelmClusterAddon{}
		if err := c.Get(ctx, types.NamespacedName{Name: addon.Name}, observed); err == nil {
			inFlight = apimeta.FindStatusCondition(
				observed.Status.Conditions, helmv1alpha1.ConditionTypeReconciling,
			)
		}
	}

	observe := interceptor.Funcs{
		Create: func(
			ctx context.Context,
			inner client.WithWatch,
			obj client.Object,
			opts ...client.CreateOption,
		) error {
			if _, isSource := obj.(*sourcev1.OCIRepository); isSource {
				captureAddonStatus(ctx)
			}

			return inner.Create(ctx, obj, opts...)
		},
		Patch: func(
			ctx context.Context,
			inner client.WithWatch,
			obj client.Object,
			patch client.Patch,
			opts ...client.PatchOption,
		) error {
			if _, isSource := obj.(*sourcev1.OCIRepository); isSource {
				captureAddonStatus(ctx)
			}

			return inner.Patch(ctx, obj, patch, opts...)
		},
	}

	r, built := newForceTestReconciler(t, observe, append(forceTestFixtures(), addon)...)
	c = built

	reconcileAddon(t, r, addon.Name)

	if inFlight == nil {
		t.Fatal("Reconciling must be published before the internal source is reconciled")
	}
	if inFlight.Status != metav1.ConditionTrue || inFlight.Reason != helmv1alpha1.ReasonForceReconcile {
		t.Fatalf("Reconciling is %s/%s, want True/%s",
			inFlight.Status, inFlight.Reason, helmv1alpha1.ReasonForceReconcile)
	}
}

func TestReconcileForcedAddonRecordsCompletion(t *testing.T) {
	addon := testAddon()
	addon.Annotations = map[string]string{helmv1alpha1.AnnotationForceReconcile: "2026-01-01T00:00:00Z"}

	r, c := newForceTestReconciler(t, interceptor.Funcs{}, append(forceTestFixtures(), addon)...)

	// metav1.Time serialises at second precision, so the stored stamp can land
	// just before an untruncated wall-clock reading of the same second.
	before := time.Now().UTC().Truncate(time.Second)

	reconcileAddon(t, r, addon.Name)

	settled := &helmv1alpha1.HelmClusterAddon{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: addon.Name}, settled); err != nil {
		t.Fatalf("getting addon: %v", err)
	}

	// The request itself is over, so the reason it raised must be gone; the pass
	// kicked off a rollout that is still running, and the progress condition is
	// handed over to the ordinary verdict rather than taken away.
	cond := apimeta.FindStatusCondition(settled.Status.Conditions, helmv1alpha1.ConditionTypeReconciling)
	if cond == nil {
		t.Fatalf("Reconciling must still report the rollout, conditions: %v", settled.Status.Conditions)
	}
	if cond.Reason == helmv1alpha1.ReasonForceReconcile {
		t.Fatal("Reconciling must stop carrying ForceReconcile once the forced pass finished")
	}
	if settled.Status.LastForceReconcileTime == nil {
		t.Fatal("lastForceReconcileTime must be recorded by the forced pass")
	}
	if settled.Status.LastForceReconcileTime.Time.Before(before) {
		t.Fatalf("lastForceReconcileTime is %v, want at or after %v",
			settled.Status.LastForceReconcileTime.Time, before)
	}
	if _, found := settled.Annotations[helmv1alpha1.AnnotationForceReconcile]; found {
		t.Fatal("the force annotation must be consumed by the pass it triggered")
	}
}

// TestReconcileUnforcedAddonRecordsNoForceReconcile is the complement: an ordinary
// pass must not report a force request that was never made.
func TestReconcileUnforcedAddonRecordsNoForceReconcile(t *testing.T) {
	addon := testAddon()

	r, c := newForceTestReconciler(t, interceptor.Funcs{}, append(forceTestFixtures(), addon)...)

	reconcileAddon(t, r, addon.Name)

	settled := &helmv1alpha1.HelmClusterAddon{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: addon.Name}, settled); err != nil {
		t.Fatalf("getting addon: %v", err)
	}

	if settled.Status.LastForceReconcileTime != nil {
		t.Fatalf("lastForceReconcileTime is %v, want it unset without a force request",
			settled.Status.LastForceReconcileTime)
	}
	if cond := apimeta.FindStatusCondition(settled.Status.Conditions, helmv1alpha1.ConditionTypeReconciling); cond != nil &&
		cond.Reason == helmv1alpha1.ReasonForceReconcile {
		t.Fatalf("an unforced pass must not report %s", helmv1alpha1.ReasonForceReconcile)
	}
}

// maintainedAddon builds an addon asking for maintenance mode, carrying a force
// request and the progress condition a forced pass publishes before it works. That
// is the state a pass interrupted between the two writes leaves behind.
func maintainedAddon() *helmv1alpha1.HelmClusterAddon {
	addon := testAddon()
	addon.Spec.Maintenance = string(helmv1alpha1.NoResourceReconciliation)
	addon.Annotations = map[string]string{helmv1alpha1.AnnotationForceReconcile: "2026-01-01T00:00:00Z"}
	addon.Status.Conditions = []metav1.Condition{{
		Type:               helmv1alpha1.ConditionTypeReconciling,
		Status:             metav1.ConditionTrue,
		Reason:             helmv1alpha1.ReasonForceReconcile,
		Message:            "Forced reconciliation in progress",
		LastTransitionTime: metav1.Now(),
	}}

	return addon
}

// TestReconcileEnteringMaintenanceDiscardsForceReconcile covers the pass that puts
// the addon into maintenance. The controller has just decided to stop reconciling
// it, so a force request it will never act on must not be left claiming progress —
// kstatus reads a standing Reconciling as work in flight.
func TestReconcileEnteringMaintenanceDiscardsForceReconcile(t *testing.T) {
	addon := maintainedAddon()

	r, c := newForceTestReconciler(t, interceptor.Funcs{}, append(forceTestFixtures(), addon)...)

	reconcileAddon(t, r, addon.Name)

	settled := &helmv1alpha1.HelmClusterAddon{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: addon.Name}, settled); err != nil {
		t.Fatalf("getting addon: %v", err)
	}

	if !settled.MaintenanceModeEnabled() {
		t.Fatalf("the fixture must reach maintenance mode first, conditions: %v", settled.Status.Conditions)
	}
	if cond := apimeta.FindStatusCondition(settled.Status.Conditions, helmv1alpha1.ConditionTypeReconciling); cond != nil {
		t.Fatalf("Reconciling must be dropped when the addon enters maintenance, got %+v", cond)
	}
	if _, found := settled.Annotations[helmv1alpha1.AnnotationForceReconcile]; found {
		t.Fatal("the force annotation must be discarded: maintenance will never act on it")
	}
	if settled.Status.LastForceReconcileTime != nil {
		t.Fatalf("lastForceReconcileTime is %v, want it unset: the request was discarded, not processed",
			settled.Status.LastForceReconcileTime)
	}
}

// TestReconcileSittingInMaintenanceDiscardsForceReconcile is the same guarantee for
// an addon already in maintenance, which takes the early return instead of the
// maintenance-change branch. Without it a request annotated onto a maintained addon
// would sit on the object forever.
func TestReconcileSittingInMaintenanceDiscardsForceReconcile(t *testing.T) {
	addon := maintainedAddon()
	addon.Status.Conditions = append(addon.Status.Conditions, metav1.Condition{
		Type:               helmv1alpha1.ConditionTypeManaged,
		Status:             metav1.ConditionFalse,
		Reason:             helmv1alpha1.ReasonMaintenanceModeActive,
		Message:            "Maintenance mode enabled",
		LastTransitionTime: metav1.Now(),
	})

	r, c := newForceTestReconciler(t, interceptor.Funcs{}, append(forceTestFixtures(), addon)...)

	if !addon.MaintenanceModeEnabled() || r.deps.Maintenance.ChangeRequired(adapter.NewAddonRelease(addon)) {
		t.Fatal("the fixture must already be in maintenance, otherwise the test takes the wrong branch")
	}

	reconcileAddon(t, r, addon.Name)

	settled := &helmv1alpha1.HelmClusterAddon{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: addon.Name}, settled); err != nil {
		t.Fatalf("getting addon: %v", err)
	}

	if cond := apimeta.FindStatusCondition(settled.Status.Conditions, helmv1alpha1.ConditionTypeReconciling); cond != nil {
		t.Fatalf("Reconciling must be dropped on a maintained addon, got %+v", cond)
	}
	if _, found := settled.Annotations[helmv1alpha1.AnnotationForceReconcile]; found {
		t.Fatal("the force annotation must be discarded: maintenance will never act on it")
	}
}

// TestReconcileLeavingMaintenanceHonoursForceReconcile is the complement. Lifting
// maintenance does not end the pass: reconciliation resumes in it, so the request
// becomes actionable here and this very pass is the one that honours it.
func TestReconcileLeavingMaintenanceHonoursForceReconcile(t *testing.T) {
	addon := testAddon()
	addon.Annotations = map[string]string{helmv1alpha1.AnnotationForceReconcile: "2026-01-01T00:00:00Z"}
	addon.Status.Conditions = []metav1.Condition{{
		Type:               helmv1alpha1.ConditionTypeManaged,
		Status:             metav1.ConditionFalse,
		Reason:             helmv1alpha1.ReasonMaintenanceModeActive,
		Message:            "Maintenance mode enabled",
		LastTransitionTime: metav1.Now(),
	}}

	r, c := newForceTestReconciler(t, interceptor.Funcs{}, append(forceTestFixtures(), addon)...)

	if addon.MaintenanceModeActivated() || !r.deps.Maintenance.ChangeRequired(adapter.NewAddonRelease(addon)) {
		t.Fatal("the fixture must be leaving maintenance, otherwise the test proves nothing")
	}

	reconcileAddon(t, r, addon.Name)

	settled := &helmv1alpha1.HelmClusterAddon{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: addon.Name}, settled); err != nil {
		t.Fatalf("getting addon: %v", err)
	}

	if _, found := settled.Annotations[helmv1alpha1.AnnotationForceReconcile]; found {
		t.Fatal("the pass that lifts maintenance resumes the work, so it consumes the request")
	}
	if settled.Status.LastForceReconcileTime == nil {
		t.Fatal("the request was acted on by the resumed pass and must be stamped")
	}
}

// TestReconcileLogsAFailureItDoesNotHandBack pins the one report a quietly failing
// pass leaves behind. Only the identity failure is handed to the work queue, which
// logs it on the way past; every other failure ends the pass with no error at all,
// and several of them report a fixed message, so without this line the cause is
// written down nowhere.
func TestReconcileLogsAFailureItDoesNotHandBack(t *testing.T) {
	addon := testAddon()
	// No repository object is created, so resolving the one the addon names fails.
	r, _ := newFullReconciler(t, &stubChartResolver{}, interceptor.Funcs{}, addon)

	var logged []string
	logger := funcr.New(func(prefix, args string) {
		logged = append(logged, args)
	}, funcr.Options{})
	ctx := log.IntoContext(context.Background(), logger)

	res, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: addon.Name}})
	if err != nil {
		t.Fatalf("Reconcile returned %v, want the failure reported rather than handed back", err)
	}
	if res.RequeueAfter != 0 {
		t.Fatalf("RequeueAfter = %v, want none: a watch on the repository wakes the addon", res.RequeueAfter)
	}

	var found bool
	for _, line := range logged {
		if strings.Contains(line, "Failed to get internal repository") &&
			strings.Contains(line, helmv1alpha1.ReasonFailed) {
			found = true
		}
	}
	if !found {
		t.Fatalf("the failure was not logged, got %q", logged)
	}
}

// internalReleaseFixture is the internal HelmRelease of an installed addon, in the
// suspend state maintenance mode leaves it in. It is what the maintenance switch
// acts on: with no such object there is nothing to suspend and the switch cannot
// fail.
func internalReleaseFixture(addon *helmv1alpha1.HelmClusterAddon, suspended bool) *helmv2.HelmRelease {
	return &helmv2.HelmRelease{
		ObjectMeta: metav1.ObjectMeta{
			Name:      adapter.NewAddonRelease(addon).InternalNames().HelmRelease,
			Namespace: helmv1alpha1.TargetNamespace,
		},
		Spec: helmv2.HelmReleaseSpec{Suspend: suspended},
	}
}

// maintainedAddonInStatus is an addon resting in maintenance: the switch was made
// and confirmed, which is the only thing that writes Managed=False.
func maintainedAddonInStatus() *helmv1alpha1.HelmClusterAddon {
	addon := testAddon()
	addon.Status.Conditions = []metav1.Condition{{
		Type:               helmv1alpha1.ConditionTypeManaged,
		Status:             metav1.ConditionFalse,
		Reason:             helmv1alpha1.ReasonMaintenanceModeActive,
		Message:            "Maintenance mode enabled",
		LastTransitionTime: metav1.Now(),
	}}

	return addon
}

// failHelmReleasePatch makes the write behind the maintenance switch fail the way a
// busy API server does: the suspend flag is read, the patch is refused.
func failHelmReleasePatch() interceptor.Funcs {
	return interceptor.Funcs{
		Patch: func(
			ctx context.Context,
			inner client.WithWatch,
			obj client.Object,
			patch client.Patch,
			opts ...client.PatchOption,
		) error {
			if _, isRelease := obj.(*helmv2.HelmRelease); isRelease {
				return errors.New("conflict: the object has been modified")
			}

			return inner.Patch(ctx, obj, patch, opts...)
		},
	}
}

// TestReconcileLeavingMaintenanceResumesTheChartInTheSamePass pins that lifting
// maintenance does not end the pass. Nothing wakes a release that is managed again —
// the spec change was already consumed by this pass — so a pass that stopped at the
// switch would leave an addon reported as managed and ready with no chart deployed.
func TestReconcileLeavingMaintenanceResumesTheChartInTheSamePass(t *testing.T) {
	addon := maintainedAddonInStatus()

	fixtures := append(forceTestFixtures(), addon, internalReleaseFixture(addon, true))
	r, c := newForceTestReconciler(t, interceptor.Funcs{}, fixtures...)

	reconcileAddon(t, r, addon.Name)

	names := adapter.NewAddonRelease(addon).InternalNames()

	internal := &helmv2.HelmRelease{}
	releaseKey := client.ObjectKey{Name: names.HelmRelease, Namespace: helmv1alpha1.TargetNamespace}
	if err := c.Get(context.Background(), releaseKey, internal); err != nil {
		t.Fatalf("getting internal release: %v", err)
	}
	if internal.Spec.Suspend {
		t.Fatal("the internal release must be resumed before Managed may say the addon is managed")
	}

	sourceKey := client.ObjectKey{Name: names.OCIRepository, Namespace: helmv1alpha1.TargetNamespace}
	if err := c.Get(context.Background(), sourceKey, &sourcev1.OCIRepository{}); err != nil {
		t.Fatalf("the pass must go on to the chart after lifting maintenance: %v", err)
	}

	settled := &helmv1alpha1.HelmClusterAddon{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: addon.Name}, settled); err != nil {
		t.Fatalf("getting addon: %v", err)
	}

	managed := apimeta.FindStatusCondition(settled.Status.Conditions, helmv1alpha1.ConditionTypeManaged)
	if managed == nil || managed.Status != metav1.ConditionTrue {
		t.Fatalf("Managed = %+v, want True once the release is resumed", managed)
	}
	ready := apimeta.FindStatusCondition(settled.Status.Conditions, helmv1alpha1.ConditionTypeReady)
	if ready == nil || ready.Status == metav1.ConditionTrue {
		t.Fatalf("Ready = %+v, want the verdict of the resumed work, which has not deployed yet", ready)
	}
}

// TestReconcileFailedMaintenanceEntryIsRetried pins that a switch that did not land
// leaves Managed where it was. Managed=False is read as "resting in maintenance" by
// everything that asks, including the check that decides whether a switch is still
// required, so writing it from a failed attempt would end the retries for good.
func TestReconcileFailedMaintenanceEntryIsRetried(t *testing.T) {
	addon := testAddon()
	addon.Spec.Maintenance = string(helmv1alpha1.NoResourceReconciliation)

	fixtures := append(forceTestFixtures(), addon, internalReleaseFixture(addon, false))
	r, c := newForceTestReconciler(t, failHelmReleasePatch(), fixtures...)

	if _, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: addon.Name},
	}); err == nil {
		t.Fatal("a switch that failed must be handed to the work queue: nothing else brings it back")
	}

	settled := &helmv1alpha1.HelmClusterAddon{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: addon.Name}, settled); err != nil {
		t.Fatalf("getting addon: %v", err)
	}

	if settled.MaintenanceModeEnabled() {
		t.Fatalf("a failed entry must not look like maintenance, conditions: %v", settled.Status.Conditions)
	}
	if !r.deps.Maintenance.ChangeRequired(adapter.NewAddonRelease(settled)) {
		t.Fatal("the switch must still be required after a failed attempt")
	}

	ready := apimeta.FindStatusCondition(settled.Status.Conditions, helmv1alpha1.ConditionTypeReady)
	if ready == nil || ready.Status != metav1.ConditionFalse || ready.Reason != helmv1alpha1.ReasonFailed {
		t.Fatalf("Ready = %+v, want False/%s", ready, helmv1alpha1.ReasonFailed)
	}
	progress := apimeta.FindStatusCondition(settled.Status.Conditions, helmv1alpha1.ConditionTypeReconciling)
	if progress == nil || progress.Status != metav1.ConditionTrue {
		t.Fatalf("Reconciling = %+v, want True while the switch is being retried", progress)
	}
}

// TestReconcileFailedMaintenanceExitIsRetriedAndStaysUnready is the same guarantee
// the other way round: the addon is still suspended, so it is still unmanaged and
// still not ready, and the attempt is retried rather than reported as a resumption.
func TestReconcileFailedMaintenanceExitIsRetriedAndStaysUnready(t *testing.T) {
	addon := maintainedAddonInStatus()

	fixtures := append(forceTestFixtures(), addon, internalReleaseFixture(addon, true))
	r, c := newForceTestReconciler(t, failHelmReleasePatch(), fixtures...)

	if _, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: addon.Name},
	}); err == nil {
		t.Fatal("a switch that failed must be handed to the work queue")
	}

	settled := &helmv1alpha1.HelmClusterAddon{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: addon.Name}, settled); err != nil {
		t.Fatalf("getting addon: %v", err)
	}

	managed := apimeta.FindStatusCondition(settled.Status.Conditions, helmv1alpha1.ConditionTypeManaged)
	if managed == nil || managed.Status != metav1.ConditionFalse ||
		managed.Reason != helmv1alpha1.ReasonMaintenanceModeActive {
		t.Fatalf("Managed = %+v, want the confirmed maintenance verdict it already carried", managed)
	}
	ready := apimeta.FindStatusCondition(settled.Status.Conditions, helmv1alpha1.ConditionTypeReady)
	if ready == nil || ready.Status != metav1.ConditionFalse {
		t.Fatalf("Ready = %+v, want False while the addon is unmanaged", ready)
	}

	internal := &helmv2.HelmRelease{}
	releaseKey := client.ObjectKey{
		Name:      adapter.NewAddonRelease(addon).InternalNames().HelmRelease,
		Namespace: helmv1alpha1.TargetNamespace,
	}
	if err := c.Get(context.Background(), releaseKey, internal); err != nil {
		t.Fatalf("getting internal release: %v", err)
	}
	if !internal.Spec.Suspend {
		t.Fatal("the internal release must still be suspended: the patch was refused")
	}
}

// maintenanceProbe watches the addon at the moment the switch is attempted. The
// condition published before the write is the only thing that says the switch is
// being made, and it cannot be observed afterwards: the pass that lands it takes it
// away again.
type maintenanceProbe struct {
	inner   Maintenance
	observe func(ctx context.Context)
}

func (p maintenanceProbe) ChangeRequired(rel contract.Release) bool {
	return p.inner.ChangeRequired(rel)
}

func (p maintenanceProbe) Ensure(ctx context.Context, rel contract.Release) internalobj.MaintenanceOutcome {
	p.observe(ctx)

	return p.inner.Ensure(ctx, rel)
}

// TestReconcileMaintenanceSwitchReportsProgressBeforeItIsMade pins that an attempted
// switch says so. Whoever edited spec.maintenance is watching the object, and
// between the edit and the write on the internal release the addon is neither in
// maintenance nor out of it.
func TestReconcileMaintenanceSwitchReportsProgressBeforeItIsMade(t *testing.T) {
	addon := testAddon()
	addon.Spec.Maintenance = string(helmv1alpha1.NoResourceReconciliation)

	fixtures := append(forceTestFixtures(), addon, internalReleaseFixture(addon, false))
	r, c := newForceTestReconciler(t, interceptor.Funcs{}, fixtures...)

	var inFlight *metav1.Condition
	r.deps.Maintenance = maintenanceProbe{
		inner: r.deps.Maintenance,
		observe: func(ctx context.Context) {
			observed := &helmv1alpha1.HelmClusterAddon{}
			if err := c.Get(ctx, types.NamespacedName{Name: addon.Name}, observed); err == nil {
				inFlight = apimeta.FindStatusCondition(
					observed.Status.Conditions, helmv1alpha1.ConditionTypeReconciling,
				)
			}
		},
	}

	reconcileAddon(t, r, addon.Name)

	if inFlight == nil || inFlight.Status != metav1.ConditionTrue {
		t.Fatalf("Reconciling = %+v at the moment of the switch, want True", inFlight)
	}

	// The same pass has to end in the state the progress was announced for: the
	// internal release suspended, the addon reported unmanaged and unready, and
	// nothing left in flight.
	internal := &helmv2.HelmRelease{}
	releaseKey := client.ObjectKey{
		Name:      adapter.NewAddonRelease(addon).InternalNames().HelmRelease,
		Namespace: helmv1alpha1.TargetNamespace,
	}
	if err := c.Get(context.Background(), releaseKey, internal); err != nil {
		t.Fatalf("getting internal release: %v", err)
	}
	if !internal.Spec.Suspend {
		t.Fatal("the internal release must be suspended before Managed may say the addon is unmanaged")
	}

	settled := &helmv1alpha1.HelmClusterAddon{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: addon.Name}, settled); err != nil {
		t.Fatalf("getting addon: %v", err)
	}

	managed := apimeta.FindStatusCondition(settled.Status.Conditions, helmv1alpha1.ConditionTypeManaged)
	if managed == nil || managed.Status != metav1.ConditionFalse ||
		managed.Reason != helmv1alpha1.ReasonMaintenanceModeActive {
		t.Fatalf("Managed = %+v, want False/%s once the switch landed",
			managed, helmv1alpha1.ReasonMaintenanceModeActive)
	}
	ready := apimeta.FindStatusCondition(settled.Status.Conditions, helmv1alpha1.ConditionTypeReady)
	if ready == nil || ready.Status != metav1.ConditionFalse {
		t.Fatalf("Ready = %+v, want False: an unmanaged addon is never ready", ready)
	}
	if progress := apimeta.FindStatusCondition(
		settled.Status.Conditions, helmv1alpha1.ConditionTypeReconciling,
	); progress != nil {
		t.Fatalf("Reconciling = %+v, want it taken away: the switch is done", progress)
	}
}

// TestReconcileTransientNamespaceFailureIsRetried pins the scheduling behind a
// cluster step that failed on its way to a write. The namespace was not created, so
// there is nothing for a watch to fire on; without the work queue the addon would
// sit in its failed status until something else touched it.
func TestReconcileTransientNamespaceFailureIsRetried(t *testing.T) {
	addon := testAddon()

	refuseNamespace := interceptor.Funcs{
		Create: func(
			ctx context.Context,
			inner client.WithWatch,
			obj client.Object,
			opts ...client.CreateOption,
		) error {
			if _, isNamespace := obj.(*corev1.Namespace); isNamespace {
				return errors.New("etcdserver: request timed out")
			}

			return inner.Create(ctx, obj, opts...)
		},
	}

	r, c := newForceTestReconciler(t, refuseNamespace, append(forceTestFixtures(), addon)...)

	if _, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: addon.Name},
	}); err == nil {
		t.Fatal("a transient cluster-step failure must be handed to the work queue")
	}

	settled := &helmv1alpha1.HelmClusterAddon{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: addon.Name}, settled); err != nil {
		t.Fatalf("getting addon: %v", err)
	}

	ready := apimeta.FindStatusCondition(settled.Status.Conditions, helmv1alpha1.ConditionTypeReady)
	if ready == nil || ready.Status != metav1.ConditionFalse || ready.Reason != helmv1alpha1.ReasonFailed {
		t.Fatalf("Ready = %+v, want False/%s", ready, helmv1alpha1.ReasonFailed)
	}
	if apimeta.FindStatusCondition(settled.Status.Conditions, helmv1alpha1.ConditionTypeStalled) != nil {
		t.Fatal("a transient failure is not a stalled one")
	}
}

// TestReconcileRejectedNamespaceNameIsNotRetried is the other half of the same
// decision. A namespace name the API server refuses as invalid is refused the same
// way on every attempt: nothing about the spec changes by trying again, so the work
// queue would only log the rejection forever. The verdict is recorded as stalled
// instead, and correcting the spec is what brings the addon back.
func TestReconcileRejectedNamespaceNameIsNotRetried(t *testing.T) {
	addon := testAddon()

	rejectNamespace := interceptor.Funcs{
		Create: func(
			ctx context.Context,
			inner client.WithWatch,
			obj client.Object,
			opts ...client.CreateOption,
		) error {
			namespace, isNamespace := obj.(*corev1.Namespace)
			if !isNamespace {
				return inner.Create(ctx, obj, opts...)
			}

			return apierrors.NewInvalid(
				schema.GroupKind{Kind: "Namespace"},
				namespace.Name,
				field.ErrorList{field.Invalid(
					field.NewPath("metadata", "name"),
					namespace.Name,
					"a lowercase RFC 1123 label must consist of lower case alphanumeric characters or '-'",
				)},
			)
		},
	}

	r, c := newForceTestReconciler(t, rejectNamespace, append(forceTestFixtures(), addon)...)

	if _, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: addon.Name},
	}); err != nil {
		t.Fatalf("a refusal no retry can change must not go to the work queue: %v", err)
	}

	settled := &helmv1alpha1.HelmClusterAddon{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: addon.Name}, settled); err != nil {
		t.Fatalf("getting addon: %v", err)
	}

	stalled := apimeta.FindStatusCondition(settled.Status.Conditions, helmv1alpha1.ConditionTypeStalled)
	if stalled == nil || stalled.Status != metav1.ConditionTrue {
		t.Fatalf("Stalled = %+v, want True: the spec has to change before this can work", stalled)
	}
	ready := apimeta.FindStatusCondition(settled.Status.Conditions, helmv1alpha1.ConditionTypeReady)
	if ready == nil || ready.Status != metav1.ConditionFalse {
		t.Fatalf("Ready = %+v, want False", ready)
	}
}

// TestBadInputIsNotLoggedAsAControllerError guards the e2e log gate as much as the
// log itself. The suite fails the run on any error line a controller emits that is
// not explicitly excluded (tests/e2e/default_config.yaml), and a release naming a
// chart version its repository does not offer is answered in its status — the
// controller did nothing wrong and has nothing to retry.
func TestBadInputIsNotLoggedAsAControllerError(t *testing.T) {
	tests := []struct {
		name        string
		failure     Failure
		wantAtError bool
	}{
		{
			name: "a version the catalog does not offer is reported at info",
			failure: Failure{
				Reason:   helmv1alpha1.ReasonChartVersionNotFound,
				Message:  "Version 9.9.9 of chart podinfo is not in the catalog of repository repo",
				Err:      errChartVersionNotFound,
				BadInput: true,
			},
			wantAtError: false,
		},
		{
			name: "a failure of the controller itself stays an error",
			failure: Failure{
				Reason:  helmv1alpha1.ReasonHelmChartFailed,
				Message: "Failed to reconcile the internal chart",
				Err:     errors.New("connection refused"),
			},
			wantAtError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// funcr writes an "error"= key for Error and never for Info, so the key is
			// what tells the two levels apart in the rendered line.
			var atError, logged bool
			var line string
			logger := funcr.New(func(prefix, args string) {
				logged = true
				line = args
				if strings.Contains(args, `"error"=`) {
					atError = true
				}
			}, funcr.Options{})

			addon := testAddon()
			r, _ := newFullReconciler(t, nil, interceptor.Funcs{}, addon)
			ctx := log.IntoContext(context.Background(), logger)

			in := Inputs{
				Generation:     addon.GetGeneration(),
				Now:            time.Now().UTC(),
				ConditionTypes: addon.GetConditionTypesForUpdate(),
			}

			if _, err := r.finish(ctx, adapter.NewAddonRelease(addon), in, StepFailed{tt.failure}); err != nil && !tt.wantAtError {
				t.Fatalf("finish returned an error: %v", err)
			}

			if atError != tt.wantAtError {
				t.Fatalf("logged at error level = %v, want %v", atError, tt.wantAtError)
			}

			// Not logging at all would pass the check above and lose the one record of
			// why the release is not ready.
			if !logged {
				t.Fatal("the failure was not logged at any level")
			}
			if !strings.Contains(line, tt.failure.Message) {
				t.Fatalf("logged line %q does not carry the message %q", line, tt.failure.Message)
			}
			if !strings.Contains(line, tt.failure.Err.Error()) {
				t.Fatalf("logged line %q does not carry the cause %q", line, tt.failure.Err)
			}
		})
	}
}

// TestReconcileAnswersAVersionTheCatalogLacksWithoutPromisingARetry runs the whole
// pass, not just the verdict, for the shape a typo in the spec takes: the repository
// answered, its catalog is complete, and the version the release names is not in it.
// The release has to say that plainly — no Reconciling, nothing on the work queue —
// while staying reversible, because publishing that version to the repository must
// bring the release back without anyone touching its spec.
func TestReconcileAnswersAVersionTheCatalogLacksWithoutPromisingARetry(t *testing.T) {
	addon := testAddon()
	addon.Finalizers = []string{helmv1alpha1.FinalizerName}

	repo := helmRepositoryFixture()
	chart := addonChartFixture(
		addon.Spec.Chart.HelmClusterAddonRepository,
		addon.Spec.Chart.HelmClusterAddonChartName,
		helmv1alpha1.ChartVersion{Version: "6.7.0"},
	)

	r, c := newFullReconciler(t, nil, interceptor.Funcs{}, addon, repo, chart)

	result, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: client.ObjectKeyFromObject(addon),
	})
	if err != nil {
		t.Fatalf("a version the catalog lacks must not be handed to the work queue, got: %v", err)
	}
	if result.RequeueAfter != 0 {
		t.Fatalf("RequeueAfter = %v, want none: no timer brings a better answer", result.RequeueAfter)
	}

	var got helmv1alpha1.HelmClusterAddon
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(addon), &got); err != nil {
		t.Fatalf("reading the addon back: %v", err)
	}

	ready := apimeta.FindStatusCondition(got.Status.Conditions, helmv1alpha1.ConditionTypeReady)
	if ready == nil || ready.Status != metav1.ConditionFalse {
		t.Fatalf("Ready = %+v, want False", ready)
	}
	if ready.Reason != helmv1alpha1.ReasonChartVersionNotFound {
		t.Fatalf("Ready reason = %q, want %q", ready.Reason, helmv1alpha1.ReasonChartVersionNotFound)
	}

	if reconciling := apimeta.FindStatusCondition(got.Status.Conditions, helmv1alpha1.ConditionTypeReconciling); reconciling != nil &&
		reconciling.Status == metav1.ConditionTrue {
		t.Fatalf("Reconciling = %+v, want absent or False: nothing is on its way to that version", reconciling)
	}

	if stalled := apimeta.FindStatusCondition(got.Status.Conditions, helmv1alpha1.ConditionTypeStalled); stalled != nil &&
		stalled.Status == metav1.ConditionTrue {
		t.Fatal("Stalled must stay off: publishing the version to the repository brings the release back")
	}
}

// TestReconcileSchedulesItselfWhileTheRepositoryHasNotSyncedYet covers the one wait
// nothing wakes the release from. A release created before its repository first
// syncs — or one naming a chart the repository does not have, which never gets a
// catalog object and so never a catalog event — learns that the sync finished only
// from the repository's status, and the release watches repositories by generation,
// which a status write does not bump. Without a timer of its own the release would
// sit claiming progress until something unrelated disturbed it.
func TestReconcileSchedulesItselfWhileTheRepositoryHasNotSyncedYet(t *testing.T) {
	addon := testAddon()
	addon.Finalizers = []string{helmv1alpha1.FinalizerName}

	// The repository exists and has never finished a sync: no catalog object for
	// the chart, nothing for a watch to fire on.
	r, c := newFullReconciler(t, nil, interceptor.Funcs{}, addon, helmRepositoryFixture())

	result, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: client.ObjectKeyFromObject(addon),
	})
	if err != nil {
		t.Fatalf("waiting on a sync is not a failure of the pass, got: %v", err)
	}
	if result.RequeueAfter <= 0 {
		t.Fatal("the release scheduled no pass of its own: nothing else brings it back")
	}

	var got helmv1alpha1.HelmClusterAddon
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(addon), &got); err != nil {
		t.Fatalf("reading the addon back: %v", err)
	}

	ready := apimeta.FindStatusCondition(got.Status.Conditions, helmv1alpha1.ConditionTypeReady)
	if ready == nil || ready.Status != metav1.ConditionFalse {
		t.Fatalf("Ready = %+v, want False", ready)
	}
	if ready.Reason == helmv1alpha1.ReasonChartVersionNotFound {
		t.Fatal("an unsynced repository must not be reported as a fault of the spec")
	}
}

// TestReconcileAnswersAnApplicationTheSameWayAsAnAddon proves the verdict is a
// property of the shared reconciler rather than of the addon adapter: the namespaced
// family asks a repository in its own namespace, through different adapters, and has
// to get the same straight answer for a version the catalog does not list.
func TestReconcileAnswersAnApplicationTheSameWayAsAnAddon(t *testing.T) {
	app := testApplication()
	app.Finalizers = []string{helmv1alpha1.FinalizerName}
	app.Spec.Chart.Version = "9.9.9"

	fixtures := applicationFixtures()
	repo, ok := fixtures[0].(*helmv1alpha1.HelmApplicationRepository)
	if !ok {
		t.Fatalf("the first application fixture is %T, want a repository", fixtures[0])
	}
	// The repository has answered in full for the spec it carries, which is what
	// makes the catalog's silence about 9.9.9 final rather than early.
	repo.Status.LastSuccessfulSyncTime = &metav1.Time{Time: time.Now().UTC()}
	repo.Status.ObservedGeneration = repo.Generation

	r, c := newApplicationFullReconciler(t, nil, append(fixtures, app)...)

	result, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Namespace: app.Namespace, Name: app.Name},
	})
	if err != nil {
		t.Fatalf("a version the catalog lacks must not be handed to the work queue, got: %v", err)
	}
	if result.RequeueAfter != 0 {
		t.Fatalf("RequeueAfter = %v, want none: no timer brings a better answer", result.RequeueAfter)
	}

	var got helmv1alpha1.HelmApplication
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(app), &got); err != nil {
		t.Fatalf("reading the application back: %v", err)
	}

	ready := apimeta.FindStatusCondition(got.Status.Conditions, helmv1alpha1.ConditionTypeReady)
	if ready == nil || ready.Status != metav1.ConditionFalse || ready.Reason != helmv1alpha1.ReasonChartVersionNotFound {
		t.Fatalf("Ready = %+v, want False with reason %q", ready, helmv1alpha1.ReasonChartVersionNotFound)
	}

	if reconciling := apimeta.FindStatusCondition(got.Status.Conditions, helmv1alpha1.ConditionTypeReconciling); reconciling != nil &&
		reconciling.Status == metav1.ConditionTrue {
		t.Fatalf("Reconciling = %+v, want absent or False", reconciling)
	}
}
