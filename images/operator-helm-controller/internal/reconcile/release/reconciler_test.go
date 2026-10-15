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
	coordinationv1 "k8s.io/api/coordination/v1"
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
	"github.com/deckhouse/operator-helm/internal/reconcile/lifecycle"
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

func TestGetHelmClusterAddonChart(t *testing.T) {
	addon := testAddon()

	tests := []struct {
		name           string
		version        helmv1alpha1.ChartVersion
		repoType       chartsource.Kind
		staleRepo      bool
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
			name: "the same empty media type entry passes for a helm repository",
			version: helmv1alpha1.ChartVersion{
				Version:           "6.7.1",
				UnavailableReason: helmv1alpha1.UnavailableReasonUnsupportedMediaType,
			},
			repoType: chartsource.Helm,
		},
		{
			name: "oci-era entry with a media type still passes right after switching to a helm repository",
			version: helmv1alpha1.ChartVersion{
				Version:   "6.7.1",
				MediaType: "application/vnd.cncf.helm.chart.content.v1.tar+gzip",
			},
			repoType: chartsource.Helm,
		},
		{
			name: "helm-era entry with no media type is rejected right after switching to an oci repository",
			version: helmv1alpha1.ChartVersion{
				Version: "6.7.1",
			},
			repoType:       chartsource.OCI,
			wantErr:        true,
			wantErrContain: "has not resolved it yet",
		},
		{
			name: "a pending verdict over resolved metadata does not hold the release",
			version: helmv1alpha1.ChartVersion{
				Version:            "6.7.1",
				OCIRef:             "oci://registry.example/charts/app:6.7.1",
				MediaType:          "application/vnd.cncf.helm.config.v1+json",
				UnavailableReason:  helmv1alpha1.UnavailableReasonResolvePending,
				UnavailableMessage: "the manifest request failed",
			},
			repoType: chartsource.OCI,
			wantErr:  false,
		},
		{
			name:      "a version already in a catalog of a previous spec is deployed",
			version:   helmv1alpha1.ChartVersion{Version: "6.7.1"},
			repoType:  chartsource.Helm,
			staleRepo: true,
			wantErr:   false,
		},
		{
			name:           "a version the addon does not reference is rejected",
			version:        helmv1alpha1.ChartVersion{Version: "9.9.9"},
			repoType:       chartsource.OCI,
			wantErr:        true,
			wantErrContain: `does not have version "6.7.1"`,
		},
		{
			name: "helm repository version published in a registry passes without a media type",
			version: helmv1alpha1.ChartVersion{
				Version: "6.7.1",
				OCIRef:  "oci://registry.example.com/charts/podinfo:6.7.1",
			},
			repoType: chartsource.Helm,
		},
		{
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

			repo := syncedRepositoryFixture()
			if tt.staleRepo {
				repo.Generation++
				repo.Status.ObservedGeneration = repo.Generation
			}

			gotVersion, err := r.getChartVersion(context.Background(), adapter.NewAddonCatalog(c, c), adapter.NewAddonRepository(repo), adapter.NewAddonRelease(addon), tt.repoType)

			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got version %+v", gotVersion)
				}
				if !strings.Contains(err.Error(), tt.wantErrContain) {
					t.Fatalf("error %q does not contain %q", err.Error(), tt.wantErrContain)
				}
				if gotVersion != nil {
					t.Fatalf("expected nil version on error, got %v", gotVersion)
				}

				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
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

func TestResolveSourceTellsAMissingVersionApartFromAnUnsyncedCatalog(t *testing.T) {
	addon := testAddon()

	tests := []struct {
		name             string
		versions         []helmv1alpha1.ChartVersion
		noCatalog        bool
		synced           bool
		staleSync        bool
		failedResync     bool
		incomplete       bool
		wantReason       string
		wantBadInput     bool
		wantRetried      bool
		wantRequeueAfter time.Duration
	}{
		{
			name:             "before the first sync an absent catalog object is still progressing",
			noCatalog:        true,
			synced:           false,
			wantReason:       helmv1alpha1.ReasonChartFetchFailed,
			wantBadInput:     false,
			wantRetried:      false,
			wantRequeueAfter: awaitSyncInterval,
		},
		{
			name:             "before the first sync an empty catalog is still progressing",
			versions:         nil,
			synced:           false,
			wantReason:       helmv1alpha1.ReasonChartFetchFailed,
			wantBadInput:     false,
			wantRetried:      false,
			wantRequeueAfter: awaitSyncInterval,
		},
		{
			name:         "after a sync an absent catalog object is a fault of the spec",
			noCatalog:    true,
			synced:       true,
			wantReason:   helmv1alpha1.ReasonChartVersionNotFound,
			wantBadInput: true,
			wantRetried:  false,
		},
		{
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
			name:             "a catalog from before a spec change is not the repository's answer",
			noCatalog:        true,
			staleSync:        true,
			wantReason:       helmv1alpha1.ReasonChartFetchFailed,
			wantBadInput:     false,
			wantRetried:      false,
			wantRequeueAfter: awaitSyncInterval,
		},
		{
			name:             "a version absent from an incomplete catalog is not the repository's answer",
			versions:         []helmv1alpha1.ChartVersion{{Version: "9.9.9"}},
			synced:           true,
			incomplete:       true,
			wantReason:       helmv1alpha1.ReasonChartFetchFailed,
			wantBadInput:     false,
			wantRetried:      false,
			wantRequeueAfter: awaitSyncInterval,
		},
		{
			name:             "an empty catalog of an incomplete read is not durable",
			versions:         nil,
			synced:           true,
			incomplete:       true,
			wantReason:       helmv1alpha1.ReasonChartFetchFailed,
			wantBadInput:     false,
			wantRetried:      false,
			wantRequeueAfter: awaitSyncInterval,
		},
		{
			name:         "a failed pass does not unpublish the catalog it did not touch",
			versions:     []helmv1alpha1.ChartVersion{{Version: "9.9.9"}},
			failedResync: true,
			wantReason:   helmv1alpha1.ReasonChartVersionNotFound,
			wantBadInput: true,
			wantRetried:  false,
		},
		{
			name:             "a populated catalog from before a spec change is not the answer either",
			versions:         []helmv1alpha1.ChartVersion{{Version: "9.9.9"}},
			staleSync:        true,
			wantReason:       helmv1alpha1.ReasonChartFetchFailed,
			wantBadInput:     false,
			wantRetried:      false,
			wantRequeueAfter: awaitSyncInterval,
		},
		{
			name: "a version still being resolved is progress, not bad input",
			versions: []helmv1alpha1.ChartVersion{{
				Version:            "6.7.1",
				UnavailableReason:  helmv1alpha1.UnavailableReasonResolvePending,
				UnavailableMessage: "the manifest request failed",
			}},
			synced:           true,
			wantReason:       helmv1alpha1.ReasonChartFetchFailed,
			wantBadInput:     false,
			wantRetried:      false,
			wantRequeueAfter: awaitSyncInterval,
		},
		{
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
				repo.Status.ObservedGeneration = repo.Generation
			}
			if tt.incomplete {
				repo.Status.UnexaminedVersions = 400
			}
			if tt.failedResync {
				repo = syncedRepositoryFixture()
				apimeta.SetStatusCondition(&repo.Status.Conditions, metav1.Condition{
					Type:               helmv1alpha1.ConditionTypeSynced,
					Status:             metav1.ConditionFalse,
					Reason:             helmv1alpha1.ReasonSyncFailed,
					Message:            "the index request failed",
					ObservedGeneration: repo.Generation,
					LastTransitionTime: metav1.Now(),
				})
			}

			_, _, failure := r.resolveSource(
				context.Background(),
				adapter.NewAddonRelease(addon),
				adapter.NewAddonRepository(repo),
				adapter.NewAddonCatalog(c, c),
				chartsource.Helm,
			)

			if failure == nil {
				t.Fatal("expected the version resolution to fail")
			}
			if failure.RequeueAfter != tt.wantRequeueAfter {
				t.Fatalf("RequeueAfter = %v, want %v", failure.RequeueAfter, tt.wantRequeueAfter)
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

	gotVersion, err := r.getChartVersion(context.Background(), adapter.NewAddonCatalog(c, c), adapter.NewAddonRepository(helmRepositoryFixture()), adapter.NewAddonRelease(addon), chartsource.OCI)
	if err == nil {
		t.Fatalf("expected an error when the addon chart does not exist, got version %+v", gotVersion)
	}
	if gotVersion != nil {
		t.Fatalf("expected nil version on error, got %v", gotVersion)
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

func syncedRepositoryFixture() *helmv1alpha1.HelmClusterAddonRepository {
	repo := helmRepositoryFixture()
	repo.Status.LastSuccessfulSyncTime = &metav1.Time{Time: time.Now().UTC()}
	repo.Status.ObservedGeneration = repo.Generation
	repo.Status.LastSyncedGeneration = repo.Generation

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
		Namespaces:      namespace.NewExistingTarget(c),
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
				Name:      naming.HelmApplicationChartName("stable", "podinfo"),
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
	if err := c.Update(context.Background(), chart); err != nil {
		t.Fatalf("updating internal helm chart status: %v", err)
	}
}

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

func TestReconcileApplicationAppliesTheChartAsItsOwnIdentity(t *testing.T) {
	app := testApplication()

	r, c := newApplicationFullReconciler(t, nil, append(applicationFixtures(), app)...)

	names := adapter.NewApplicationRelease(app).InternalNames()

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
	if len(roles.Items) != 0 {
		t.Fatalf("roles = %+v, want none: the rights come from the module's ClusterRole", roles.Items)
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

func (failingIdentity) Cleanup(context.Context, contract.Release) (status.DeletingResource, error) {
	return nil, nil
}

func (failingIdentity) RemoveAccount(context.Context, contract.Release) error { return nil }

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

func (a *intermittentIdentity) Cleanup(ctx context.Context, rel contract.Release) (status.DeletingResource, error) {
	return a.real.Cleanup(ctx, rel)
}

func (a *intermittentIdentity) RemoveAccount(ctx context.Context, rel contract.Release) error {
	return a.real.RemoveAccount(ctx, rel)
}

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

func TestReconcileApplicationStallsOnAForeignRoleBinding(t *testing.T) {
	app := testApplication()
	names := adapter.NewApplicationRelease(app).InternalNames()
	foreign := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: names.ServiceAccount, Namespace: app.Namespace},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: "someone-elses-role"},
		Subjects:   []rbacv1.Subject{{Kind: rbacv1.UserKind, Name: "someone-else"}},
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

	stored := &rbacv1.RoleBinding{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(foreign), stored); err != nil {
		t.Fatalf("the foreign role binding must survive: %v", err)
	}
	if stored.RoleRef != foreign.RoleRef || !reflect.DeepEqual(stored.Subjects, foreign.Subjects) {
		t.Fatalf("binding = %+v, want it untouched", stored)
	}

	releaseKey := client.ObjectKey{Name: names.HelmRelease, Namespace: helmv1alpha1.TargetNamespace}
	if err := c.Get(context.Background(), releaseKey, &helmv2.HelmRelease{}); err == nil {
		t.Fatal("no release must be created for an application whose identity could not be built")
	}
}

type accessCleanupFails struct{}

func (accessCleanupFails) Ensure(context.Context, contract.Release) rbac.IdentityOutcome {
	return rbac.IdentityOutcome{}
}

func (accessCleanupFails) Cleanup(context.Context, contract.Release) (status.DeletingResource, error) {
	return nil, errors.New("role binding deletion forbidden")
}

func (accessCleanupFails) RemoveAccount(context.Context, contract.Release) error { return nil }

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
			name: "same version but a different chart name is not a flip",
			last: &helmv1alpha1.HelmClusterAddonLastAppliedChartRef{
				HelmClusterAddonRepository: "example",
				HelmClusterAddonChartName:  "other-chart",
				Version:                    "6.7.1",
			},
			wantLogged: false,
		},
		{
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

func TestReconcileForcedAddonReportsProgressBeforeWorking(t *testing.T) {
	addon := testAddon()
	addon.Annotations = map[string]string{helmv1alpha1.AnnotationForceReconcile: "2026-01-01T00:00:00Z"}

	var inFlight *metav1.Condition
	var c client.Client

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

	before := time.Now().UTC().Truncate(time.Second)

	reconcileAddon(t, r, addon.Name)

	settled := &helmv1alpha1.HelmClusterAddon{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: addon.Name}, settled); err != nil {
		t.Fatalf("getting addon: %v", err)
	}

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

func TestReconcileLogsAFailureItDoesNotHandBack(t *testing.T) {
	addon := testAddon()
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

func internalReleaseFixture(addon *helmv1alpha1.HelmClusterAddon, suspended bool) *helmv2.HelmRelease {
	return &helmv2.HelmRelease{
		ObjectMeta: metav1.ObjectMeta{
			Name:      adapter.NewAddonRelease(addon).InternalNames().HelmRelease,
			Namespace: helmv1alpha1.TargetNamespace,
		},
		Spec: helmv2.HelmReleaseSpec{Suspend: suspended},
	}
}

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
			name: "a wait for the repository's first sync is reported at info",
			failure: Failure{
				Reason:       helmv1alpha1.ReasonChartFetchFailed,
				Message:      "Failed to resolve the desired chart version",
				Err:          errCatalogAwaited,
				AwaitsWatch:  true,
				RequeueAfter: awaitSyncInterval,
			},
			wantAtError: false,
		},
		{
			name: "a repository that is not there yet is reported at info",
			failure: Failure{
				Reason:      helmv1alpha1.ReasonFailed,
				Message:     "Failed to get internal repository",
				Err:         errors.New("getting internal repository: not found"),
				AwaitsWatch: true,
			},
			wantAtError: false,
		},
		{
			name: "a chart claimed by another release is reported at info",
			failure: Failure{
				Reason:       helmv1alpha1.ReasonChartClaimConflict,
				Message:      "chart podinfo is already used by helmclusteraddon/other",
				RequeueAfter: chartClaimConflictRequeueInterval,
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

			if !logged {
				t.Fatal("the failure was not logged at any level")
			}
			if !strings.Contains(line, tt.failure.Message) {
				t.Fatalf("logged line %q does not carry the message %q", line, tt.failure.Message)
			}
			if tt.failure.Err != nil && !strings.Contains(line, tt.failure.Err.Error()) {
				t.Fatalf("logged line %q does not carry the cause %q", line, tt.failure.Err)
			}
			if tt.failure.Err == nil && strings.Contains(line, `"cause"=`) {
				t.Fatalf("logged line %q carries a cause the failure does not have", line)
			}
		})
	}
}

func TestReconcileAnswersAVersionTheCatalogLacksWithoutPromisingARetry(t *testing.T) {
	addon := testAddon()
	addon.Finalizers = []string{helmv1alpha1.FinalizerName}

	repo := syncedRepositoryFixture()
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

func TestReconcileSchedulesItselfWhileTheRepositoryHasNotSyncedYet(t *testing.T) {
	addon := testAddon()
	addon.Finalizers = []string{helmv1alpha1.FinalizerName}

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

func TestReconcileAnswersAnApplicationTheSameWayAsAnAddon(t *testing.T) {
	app := testApplication()
	app.Finalizers = []string{helmv1alpha1.FinalizerName}
	app.Spec.Chart.Version = "9.9.9"

	fixtures := applicationFixtures()
	repo, ok := fixtures[0].(*helmv1alpha1.HelmApplicationRepository)
	if !ok {
		t.Fatalf("the first application fixture is %T, want a repository", fixtures[0])
	}
	repo.Status.LastSuccessfulSyncTime = &metav1.Time{Time: time.Now().UTC()}
	repo.Status.ObservedGeneration = repo.Generation
	repo.Status.LastSyncedGeneration = repo.Generation

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

func TestReconcileDeleteHoldsTheFinalizerUntilTheIdentityIsGone(t *testing.T) {
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
	names := adapter.NewApplicationRelease(app).InternalNames()

	binding := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:       names.ServiceAccount,
			Namespace:  app.Namespace,
			Labels:     map[string]string{helmv1alpha1.LabelManagedBy: helmv1alpha1.LabelManagedByValue},
			Finalizers: []string{"policy.example.com/protect"},
		},
		RoleRef: applicationClusterRoleRef(),
	}
	account := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{
		Name: names.ServiceAccount, Namespace: helmv1alpha1.TargetNamespace,
	}}

	r, c := newApplicationFullReconciler(t, nil, app, binding, account)

	key := types.NamespacedName{Namespace: app.Namespace, Name: app.Name}
	ctx := context.Background()

	for attempt := 1; attempt <= 2; attempt++ {
		result, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		if err != nil {
			t.Fatalf("attempt %d: Reconcile returned %v", attempt, err)
		}
		if result.RequeueAfter != lifecycle.InternalResourceDeletionRequeueInterval {
			t.Fatalf("attempt %d: requeueAfter = %v, want %v",
				attempt, result.RequeueAfter, lifecycle.InternalResourceDeletionRequeueInterval)
		}

		settled := &helmv1alpha1.HelmApplication{}
		if err := c.Get(ctx, key, settled); err != nil {
			t.Fatalf("attempt %d: getting application: %v", attempt, err)
		}
		if !controllerutil.ContainsFinalizer(settled, helmv1alpha1.FinalizerName) {
			t.Fatalf("attempt %d: the finalizer must stay while the role binding is terminating", attempt)
		}
	}

	held := &rbacv1.RoleBinding{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(binding), held); err != nil {
		t.Fatalf("getting the terminating role binding: %v", err)
	}
	held.Finalizers = nil
	if err := c.Update(ctx, held); err != nil {
		t.Fatalf("releasing the foreign finalizer: %v", err)
	}

	if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key}); err != nil {
		t.Fatalf("final Reconcile returned %v", err)
	}

	if err := c.Get(ctx, key, &helmv1alpha1.HelmApplication{}); !apierrors.IsNotFound(err) {
		t.Fatalf("the application must be gone once its identity is, got %v", err)
	}
	if err := c.Get(ctx, client.ObjectKey{
		Namespace: helmv1alpha1.TargetNamespace, Name: names.ServiceAccount,
	}, &corev1.ServiceAccount{}); !apierrors.IsNotFound(err) {
		t.Fatalf("the service account must be gone, got %v", err)
	}
}

func deletingApplicationFixtures(namespaceTerminating bool) (*helmv1alpha1.HelmApplication, []client.Object) {
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
	names := adapter.NewApplicationRelease(app).InternalNames()

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: app.Namespace}}
	if namespaceTerminating {
		ns.Finalizers = []string{"kubernetes"}
		ns.DeletionTimestamp = &now
	}

	return app, []client.Object{
		app,
		ns,
		&helmv2.HelmRelease{ObjectMeta: metav1.ObjectMeta{
			Name:       names.HelmRelease,
			Namespace:  helmv1alpha1.TargetNamespace,
			Finalizers: []string{"finalizers.fluxcd.io"},
		}},
		&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{
			Name: names.ServiceAccount, Namespace: helmv1alpha1.TargetNamespace,
		}},
		&rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{
				Name:      names.ServiceAccount,
				Namespace: app.Namespace,
				Labels:    map[string]string{helmv1alpha1.LabelManagedBy: helmv1alpha1.LabelManagedByValue},
			},
			RoleRef: applicationClusterRoleRef(),
		},
	}
}

func applicationClusterRoleRef() rbacv1.RoleRef {
	return rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: rbac.ApplicationClusterRoleName}
}

func objectExists(t *testing.T, c client.Client, key client.ObjectKey, obj client.Object) bool {
	t.Helper()

	switch err := c.Get(context.Background(), key, obj); {
	case err == nil:
		return true
	case apierrors.IsNotFound(err):
		return false
	default:
		t.Fatalf("getting %T %s: %v", obj, key, err)
		return false
	}
}

func releaseHelmControllerFinalizer(t *testing.T, c client.Client, key client.ObjectKey) {
	t.Helper()

	release := &helmv2.HelmRelease{}
	if err := c.Get(context.Background(), key, release); err != nil {
		t.Fatalf("getting the terminating internal release: %v", err)
	}

	release.Finalizers = nil
	if err := c.Update(context.Background(), release); err != nil {
		t.Fatalf("releasing the internal release's finalizer: %v", err)
	}
}

func TestReconcileDeleteRemovesTheServiceAccountFirstWhenTheNamespaceIsTerminating(t *testing.T) {
	app, objects := deletingApplicationFixtures(true)
	names := adapter.NewApplicationRelease(app).InternalNames()

	r, c := newApplicationFullReconciler(t, nil, objects...)

	ctx := context.Background()
	key := client.ObjectKeyFromObject(app)
	accountKey := client.ObjectKey{Namespace: helmv1alpha1.TargetNamespace, Name: names.ServiceAccount}
	releaseKey := client.ObjectKey{Namespace: helmv1alpha1.TargetNamespace, Name: names.HelmRelease}

	result, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
	if err != nil {
		t.Fatalf("Reconcile returned %v", err)
	}
	if result.RequeueAfter != lifecycle.InternalResourceDeletionRequeueInterval {
		t.Fatalf("requeueAfter = %v, want %v", result.RequeueAfter, lifecycle.InternalResourceDeletionRequeueInterval)
	}

	if objectExists(t, c, accountKey, &corev1.ServiceAccount{}) {
		t.Fatal("the service account must be gone before the wait on the internal release, or the uninstall hangs on it")
	}

	internalRelease := &helmv2.HelmRelease{}
	if !objectExists(t, c, releaseKey, internalRelease) {
		t.Fatal("the internal release must still be there: this pass is the one that waits for it")
	}
	if internalRelease.DeletionTimestamp.IsZero() {
		t.Fatal("the internal release must have been asked to go away in the same pass")
	}

	settled := &helmv1alpha1.HelmApplication{}
	if err := c.Get(ctx, key, settled); err != nil {
		t.Fatalf("getting application: %v", err)
	}
	if !controllerutil.ContainsFinalizer(settled, helmv1alpha1.FinalizerName) {
		t.Fatal("the finalizer must stay while the internal release is still there")
	}

	if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key}); err != nil {
		t.Fatalf("second Reconcile returned %v", err)
	}
	if objectExists(t, c, accountKey, &corev1.ServiceAccount{}) {
		t.Fatal("a later pass of the same teardown put the service account back")
	}
	if !objectExists(t, c, releaseKey, &helmv2.HelmRelease{}) {
		t.Fatal("the internal release must still be there: the pass must still be the waiting one")
	}

	releaseHelmControllerFinalizer(t, c, releaseKey)

	binding := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{
		Name: names.ServiceAccount, Namespace: app.Namespace,
	}}
	if err := c.Delete(ctx, binding); err != nil {
		t.Fatalf("deleting the role binding the namespace controller collects: %v", err)
	}

	if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key}); err != nil {
		t.Fatalf("final Reconcile returned %v", err)
	}

	if objectExists(t, c, key, &helmv1alpha1.HelmApplication{}) {
		t.Fatal("the teardown must complete once the internal release and the identity are gone")
	}
}

func TestReconcileDeleteKeepsTheServiceAccountUntilTheInternalReleaseIsGone(t *testing.T) {
	app, objects := deletingApplicationFixtures(false)
	names := adapter.NewApplicationRelease(app).InternalNames()

	r, c := newApplicationFullReconciler(t, nil, objects...)

	ctx := context.Background()
	key := client.ObjectKeyFromObject(app)
	accountKey := client.ObjectKey{Namespace: helmv1alpha1.TargetNamespace, Name: names.ServiceAccount}
	bindingKey := client.ObjectKey{Namespace: app.Namespace, Name: names.ServiceAccount}
	releaseKey := client.ObjectKey{Namespace: helmv1alpha1.TargetNamespace, Name: names.HelmRelease}

	if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key}); err != nil {
		t.Fatalf("Reconcile returned %v", err)
	}

	if !objectExists(t, c, accountKey, &corev1.ServiceAccount{}) {
		t.Fatal("the service account must outlive the internal release: helm-controller uninstalls as it")
	}
	if !objectExists(t, c, bindingKey, &rbacv1.RoleBinding{}) {
		t.Fatal("the role binding must outlive the internal release: without it the account may do nothing")
	}

	releaseHelmControllerFinalizer(t, c, releaseKey)

	for attempt := 1; attempt <= 3 && objectExists(t, c, key, &helmv1alpha1.HelmApplication{}); attempt++ {
		if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key}); err != nil {
			t.Fatalf("attempt %d: Reconcile returned %v", attempt, err)
		}
	}

	if objectExists(t, c, key, &helmv1alpha1.HelmApplication{}) {
		t.Fatal("the teardown must complete once the internal release is gone")
	}
	if objectExists(t, c, accountKey, &corev1.ServiceAccount{}) {
		t.Fatal("the service account must be gone by the end of the teardown")
	}
	if objectExists(t, c, bindingKey, &rbacv1.RoleBinding{}) {
		t.Fatal("the role binding must be gone by the end of the teardown")
	}
}

func TestReconcileDeleteRepairsARoleBindingRemovedWhileTheUninstallIsInFlight(t *testing.T) {
	app, objects := deletingApplicationFixtures(false)
	names := adapter.NewApplicationRelease(app).InternalNames()

	r, c := newApplicationFullReconciler(t, nil, objects...)

	ctx := context.Background()
	key := client.ObjectKeyFromObject(app)
	bindingKey := client.ObjectKey{Namespace: app.Namespace, Name: names.ServiceAccount}
	releaseKey := client.ObjectKey{Namespace: helmv1alpha1.TargetNamespace, Name: names.HelmRelease}

	if err := c.Delete(ctx, &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{
		Name: names.ServiceAccount, Namespace: app.Namespace,
	}}); err != nil {
		t.Fatalf("deleting the role binding out of band: %v", err)
	}

	result, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
	if err != nil {
		t.Fatalf("Reconcile returned %v", err)
	}
	if result.RequeueAfter != lifecycle.InternalResourceDeletionRequeueInterval {
		t.Fatalf("requeueAfter = %v, want %v", result.RequeueAfter, lifecycle.InternalResourceDeletionRequeueInterval)
	}

	repaired := &rbacv1.RoleBinding{}
	if !objectExists(t, c, bindingKey, repaired) {
		t.Fatal("the role binding must be back: without it the uninstall is refused and the teardown never ends")
	}
	if repaired.RoleRef != applicationClusterRoleRef() {
		t.Fatalf("roleRef = %+v, want the application ClusterRole", repaired.RoleRef)
	}
	wantSubjects := []rbacv1.Subject{{
		Kind: rbacv1.ServiceAccountKind, Name: names.ServiceAccount, Namespace: helmv1alpha1.TargetNamespace,
	}}
	if !reflect.DeepEqual(repaired.Subjects, wantSubjects) {
		t.Fatalf("subjects = %+v, want %+v", repaired.Subjects, wantSubjects)
	}
	if repaired.Labels[helmv1alpha1.LabelManagedBy] != helmv1alpha1.LabelManagedByValue {
		t.Fatalf("labels = %+v, want the managed-by label the watches select on", repaired.Labels)
	}

	if !objectExists(t, c, releaseKey, &helmv2.HelmRelease{}) {
		t.Fatal("the internal release must still be there: this pass is the one that waits for it")
	}

	settled := &helmv1alpha1.HelmApplication{}
	if err := c.Get(ctx, key, settled); err != nil {
		t.Fatalf("getting application: %v", err)
	}
	if !controllerutil.ContainsFinalizer(settled, helmv1alpha1.FinalizerName) {
		t.Fatal("the finalizer must stay while the internal release is still there")
	}
	uninstalling := apimeta.FindStatusCondition(settled.Status.Conditions, helmv1alpha1.ConditionTypeReady)
	if uninstalling == nil || uninstalling.Status != metav1.ConditionUnknown {
		t.Fatalf("Ready = %+v, want Unknown: the repair succeeded and the wait goes on", uninstalling)
	}
}

type ensureCountingIdentity struct {
	Identity
	ensureCalls int
}

func (s *ensureCountingIdentity) Ensure(ctx context.Context, rel contract.Release) rbac.IdentityOutcome {
	s.ensureCalls++

	return s.Identity.Ensure(ctx, rel)
}

func TestReconcileDeleteOfAnAddonRepairsNoIdentity(t *testing.T) {
	now := metav1.Now()
	addon := testAddon()
	addon.Finalizers = []string{helmv1alpha1.FinalizerName}
	addon.DeletionTimestamp = &now

	names := adapter.NewAddonRelease(addon).InternalNames()

	r, c := newFullReconciler(t, nil, interceptor.Funcs{},
		addon,
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: addon.Spec.Namespace}},
		&helmv2.HelmRelease{ObjectMeta: metav1.ObjectMeta{
			Name:       names.HelmRelease,
			Namespace:  helmv1alpha1.TargetNamespace,
			Finalizers: []string{"finalizers.fluxcd.io"},
		}},
	)

	identity := &ensureCountingIdentity{Identity: rbac.NewIdentity(c, helmv1alpha1.TargetNamespace)}
	r.deps.Identity = identity

	ctx := context.Background()

	result, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(addon)})
	if err != nil {
		t.Fatalf("Reconcile returned %v", err)
	}
	if result.RequeueAfter != lifecycle.InternalResourceDeletionRequeueInterval {
		t.Fatalf("requeueAfter = %v, want %v", result.RequeueAfter, lifecycle.InternalResourceDeletionRequeueInterval)
	}

	if identity.ensureCalls != 1 {
		t.Fatalf("Ensure was called %d times, want once: the repair is not conditional on the family",
			identity.ensureCalls)
	}

	accounts := &corev1.ServiceAccountList{}
	if err := c.List(ctx, accounts); err != nil {
		t.Fatalf("listing service accounts: %v", err)
	}
	roles := &rbacv1.RoleList{}
	if err := c.List(ctx, roles); err != nil {
		t.Fatalf("listing roles: %v", err)
	}
	bindings := &rbacv1.RoleBindingList{}
	if err := c.List(ctx, bindings); err != nil {
		t.Fatalf("listing role bindings: %v", err)
	}

	if len(accounts.Items)+len(roles.Items)+len(bindings.Items) != 0 {
		t.Fatalf("the addon's teardown created %d accounts, %d roles and %d bindings, want none",
			len(accounts.Items), len(roles.Items), len(bindings.Items))
	}
}

func TestReconcileDeleteKeepsWaitingWhenTheIdentityRepairIsBlockedByAForeignObject(t *testing.T) {
	app, objects := deletingApplicationFixtures(false)
	names := adapter.NewApplicationRelease(app).InternalNames()

	r, c := newApplicationFullReconciler(t, nil, objects...)

	ctx := context.Background()
	key := client.ObjectKeyFromObject(app)
	bindingKey := client.ObjectKey{Namespace: app.Namespace, Name: names.ServiceAccount}
	releaseKey := client.ObjectKey{Namespace: helmv1alpha1.TargetNamespace, Name: names.HelmRelease}

	if err := c.Delete(ctx, &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{
		Name: names.ServiceAccount, Namespace: app.Namespace,
	}}); err != nil {
		t.Fatalf("deleting the role binding out of band: %v", err)
	}
	foreign := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: names.ServiceAccount, Namespace: app.Namespace},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: "someone-elses-role"},
	}
	if err := c.Create(ctx, foreign); err != nil {
		t.Fatalf("creating the foreign role binding: %v", err)
	}

	for attempt := 1; attempt <= 2; attempt++ {
		result, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		if err != nil {
			t.Fatalf("attempt %d: a terminal repair failure must not be handed to the work queue: %v", attempt, err)
		}
		if result.RequeueAfter != lifecycle.InternalResourceDeletionRequeueInterval {
			t.Fatalf("attempt %d: requeueAfter = %v, want %v",
				attempt, result.RequeueAfter, lifecycle.InternalResourceDeletionRequeueInterval)
		}
	}

	settled := &helmv1alpha1.HelmApplication{}
	if err := c.Get(ctx, key, settled); err != nil {
		t.Fatalf("getting application: %v", err)
	}
	if !controllerutil.ContainsFinalizer(settled, helmv1alpha1.FinalizerName) {
		t.Fatal("the finalizer must stay: the internal release is still there")
	}

	ready := apimeta.FindStatusCondition(settled.Status.Conditions, helmv1alpha1.ConditionTypeReady)
	if ready == nil || ready.Status != metav1.ConditionFalse || ready.Reason != helmv1alpha1.ReasonFailed {
		t.Fatalf("Ready = %+v, want False/%s", ready, helmv1alpha1.ReasonFailed)
	}
	if !strings.Contains(ready.Message, "not managed by the operator") {
		t.Fatalf("Ready message = %q, want the foreign object named as what blocks the teardown", ready.Message)
	}

	if !objectExists(t, c, releaseKey, &helmv2.HelmRelease{}) {
		t.Fatal("the teardown must still be waiting on the internal release")
	}

	stored := &rbacv1.RoleBinding{}
	if !objectExists(t, c, bindingKey, stored) {
		t.Fatal("the foreign binding must survive: it is not ours to remove")
	}
	if stored.RoleRef.Name != "someone-elses-role" || len(stored.Subjects) != 0 || stored.Labels != nil {
		t.Fatalf("the foreign binding was modified: %+v", stored)
	}
}

type identityRepairFails struct {
	Identity
}

func (identityRepairFails) Ensure(context.Context, contract.Release) rbac.IdentityOutcome {
	err := errors.New("role binding create refused")

	return rbac.IdentityOutcome{
		Err:     err,
		Reason:  helmv1alpha1.ReasonRBACSetupFailed,
		Message: "Failed to set up the release identity: " + err.Error(),
	}
}

func TestReconcileDeleteRetriesATransientIdentityRepairFailure(t *testing.T) {
	app, objects := deletingApplicationFixtures(false)

	r, c := newApplicationFullReconciler(t, nil, objects...)
	r.deps.Identity = identityRepairFails{Identity: r.deps.Identity}

	ctx := context.Background()
	key := client.ObjectKeyFromObject(app)

	if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key}); err == nil {
		t.Fatal("a transient repair failure must be returned so the work queue retries it")
	}

	settled := &helmv1alpha1.HelmApplication{}
	if err := c.Get(ctx, key, settled); err != nil {
		t.Fatalf("getting application: %v", err)
	}
	if !controllerutil.ContainsFinalizer(settled, helmv1alpha1.FinalizerName) {
		t.Fatal("the finalizer must stay while the identity cannot be repaired")
	}

	ready := apimeta.FindStatusCondition(settled.Status.Conditions, helmv1alpha1.ConditionTypeReady)
	if ready == nil || ready.Status != metav1.ConditionFalse || ready.Reason != helmv1alpha1.ReasonFailed {
		t.Fatalf("Ready = %+v, want False/%s", ready, helmv1alpha1.ReasonFailed)
	}
}

type countingNamespaces struct {
	Namespaces
	terminatingCalls int
}

func (s *countingNamespaces) Terminating(ctx context.Context, rel contract.Release) (bool, error) {
	s.terminatingCalls++

	return s.Namespaces.Terminating(ctx, rel)
}

type countingIdentity struct {
	NoIdentity
	removeCalls int
}

func (s *countingIdentity) RemoveAccount(context.Context, contract.Release) error {
	s.removeCalls++

	return nil
}

func TestReconcileDeleteOfAnAddonIgnoresATerminatingTargetNamespace(t *testing.T) {
	now := metav1.Now()
	addon := testAddon()
	addon.Finalizers = []string{helmv1alpha1.FinalizerName}
	addon.DeletionTimestamp = &now

	names := adapter.NewAddonRelease(addon).InternalNames()

	r, c := newFullReconciler(t, nil, interceptor.Funcs{},
		addon,
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
			Name:              addon.Spec.Namespace,
			Finalizers:        []string{"kubernetes"},
			DeletionTimestamp: &now,
		}},
		&helmv2.HelmRelease{ObjectMeta: metav1.ObjectMeta{
			Name:       names.HelmRelease,
			Namespace:  helmv1alpha1.TargetNamespace,
			Finalizers: []string{"finalizers.fluxcd.io"},
		}},
	)

	namespaces := &countingNamespaces{Namespaces: r.deps.Namespaces}
	identity := &countingIdentity{}
	r.deps.Namespaces = namespaces
	r.deps.Identity = identity

	ctx := context.Background()
	key := client.ObjectKeyFromObject(addon)

	result, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
	if err != nil {
		t.Fatalf("Reconcile returned %v", err)
	}
	if result.RequeueAfter != lifecycle.InternalResourceDeletionRequeueInterval {
		t.Fatalf("requeueAfter = %v, want %v", result.RequeueAfter, lifecycle.InternalResourceDeletionRequeueInterval)
	}

	if identity.removeCalls != 0 {
		t.Fatalf("RemoveAccount was called %d times, want none: an addon has no identity to remove",
			identity.removeCalls)
	}
	if namespaces.terminatingCalls != 0 {
		t.Fatalf("the namespace was read %d times, want none: the answer cannot change an addon's teardown",
			namespaces.terminatingCalls)
	}

	settled := &helmv1alpha1.HelmClusterAddon{}
	if err := c.Get(ctx, key, settled); err != nil {
		t.Fatalf("getting addon: %v", err)
	}
	if !controllerutil.ContainsFinalizer(settled, helmv1alpha1.FinalizerName) {
		t.Fatal("the addon must still wait for its internal release, exactly as it always has")
	}
}

func TestReconcileReleasesAnAbandonedClaimEvenWhenTheNewPairIsTaken(t *testing.T) {
	addon := testAddon()
	addon.Spec.Chart.HelmClusterAddonChartName = "nginx"
	addon.Status.LastAppliedChart = &helmv1alpha1.HelmClusterAddonLastAppliedChartRef{
		HelmClusterAddonRepository: addon.Spec.Chart.HelmClusterAddonRepository,
		HelmClusterAddonChartName:  "nginx",
		Version:                    "6.7.1",
	}

	abandoned := claimLease(utils.GetChartClaimLeaseName(addon.Spec.Chart.HelmClusterAddonRepository, "podinfo"), addon.Name, addon.Name)
	taken := claimLease(utils.GetChartClaimLeaseName(addon.Spec.Chart.HelmClusterAddonRepository, "nginx"), "other", "other")

	r, c := newFullReconciler(t, nil, interceptor.Funcs{}, addon, &helmv1alpha1.HelmClusterAddon{
		ObjectMeta: metav1.ObjectMeta{Name: "other", Generation: 1},
	}, abandoned, taken)

	result, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: addon.Name},
	})
	if err != nil {
		t.Fatalf("reconcile returned an error: %v", err)
	}
	if result.RequeueAfter != chartClaimConflictRequeueInterval {
		t.Fatalf("requeueAfter is %v, want the claim conflict interval", result.RequeueAfter)
	}

	err = c.Get(context.Background(), types.NamespacedName{Namespace: helmv1alpha1.TargetNamespace, Name: abandoned.Name}, &coordinationv1.Lease{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("the abandoned claim survived a pass that lost the new one: %v", err)
	}

	held := &coordinationv1.Lease{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: helmv1alpha1.TargetNamespace, Name: taken.Name}, held); err != nil {
		t.Fatalf("reading the contested claim: %v", err)
	}
	if holder := held.Spec.HolderIdentity; holder == nil || *holder != "other" {
		t.Fatalf("holder on the contested lease = %v, want it untouched", holder)
	}
}

func claimLease(name, holder, labelSource string) *coordinationv1.Lease {
	return &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: helmv1alpha1.TargetNamespace,
			Labels: map[string]string{
				helmv1alpha1.LabelManagedBy:                  helmv1alpha1.LabelManagedByValue,
				helmv1alpha1.HelmClusterAddonLabelSourceName: labelSource,
			},
		},
		Spec: coordinationv1.LeaseSpec{HolderIdentity: &holder},
	}
}

func TestReconcileDeleteReleasesTheClaimsItNeverGotToGiveUp(t *testing.T) {
	addon := testAddon()
	addon.Spec.Chart.HelmClusterAddonChartName = "nginx"
	addon.DeletionTimestamp = &metav1.Time{Time: time.Now()}
	addon.Finalizers = []string{helmv1alpha1.FinalizerName}

	repo := addon.Spec.Chart.HelmClusterAddonRepository
	abandoned := claimLease(utils.GetChartClaimLeaseName(repo, "podinfo"), addon.Name, addon.Name)
	current := claimLease(utils.GetChartClaimLeaseName(repo, "nginx"), addon.Name, addon.Name)

	r, c := newFullReconciler(t, nil, interceptor.Funcs{}, addon, abandoned, current)

	if _, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: addon.Name},
	}); err != nil {
		t.Fatalf("reconcile returned an error: %v", err)
	}

	for _, lease := range []*coordinationv1.Lease{current, abandoned} {
		err := c.Get(context.Background(), types.NamespacedName{Namespace: helmv1alpha1.TargetNamespace, Name: lease.Name}, &coordinationv1.Lease{})
		if !apierrors.IsNotFound(err) {
			t.Fatalf("claim %q outlived the addon that held it: %v", lease.Name, err)
		}
	}
}
