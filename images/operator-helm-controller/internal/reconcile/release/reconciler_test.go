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
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr/funcr"
	fluxmeta "github.com/werf/3p-fluxcd-pkg/apis/meta"
	helmv2 "github.com/werf/3p-helm-controller/api/v2"
	sourcev1 "github.com/werf/nelm-source-controller/api/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/deckhouse/operator-helm/api/naming"
	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
	"github.com/deckhouse/operator-helm/internal/adapter"
	repoclient "github.com/deckhouse/operator-helm/internal/client/repository"
	"github.com/deckhouse/operator-helm/internal/index"
	"github.com/deckhouse/operator-helm/internal/manager/status"
	"github.com/deckhouse/operator-helm/internal/services"
	"github.com/deckhouse/operator-helm/internal/source"
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
		repoType       utils.InternalRepositoryType
		wantErr        bool
		wantErrContain string
	}{
		{
			name: "oci version with a media type passes",
			version: helmv1alpha1.ChartVersion{
				Version:   "6.7.1",
				MediaType: "application/vnd.cncf.helm.chart.content.v1.tar+gzip",
			},
			repoType: utils.InternalOCIRepository,
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
			repoType: utils.InternalOCIRepository,
		},
		{
			name: "oci version stuck resolving is rejected with reason and message",
			version: helmv1alpha1.ChartVersion{
				Version:            "6.7.1",
				UnavailableReason:  helmv1alpha1.UnavailableReasonResolvePending,
				UnavailableMessage: "manifest request failed",
			},
			repoType:       utils.InternalOCIRepository,
			wantErr:        true,
			wantErrContain: "ResolvePending: manifest request failed",
		},
		{
			name: "oci version with unsupported media type and no message is rejected with reason alone",
			version: helmv1alpha1.ChartVersion{
				Version:           "6.7.1",
				UnavailableReason: helmv1alpha1.UnavailableReasonUnsupportedMediaType,
			},
			repoType:       utils.InternalOCIRepository,
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
			repoType: utils.InternalHelmRepository,
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
			repoType: utils.InternalHelmRepository,
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
			repoType:       utils.InternalOCIRepository,
			wantErr:        true,
			wantErrContain: "has not resolved it yet",
		},
		{
			name:           "a version the addon does not reference is rejected",
			version:        helmv1alpha1.ChartVersion{Version: "9.9.9"},
			repoType:       utils.InternalOCIRepository,
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
			repoType: utils.InternalHelmRepository,
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
			repoType:       utils.InternalHelmRepository,
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

func TestGetHelmClusterAddonChartMissingChart(t *testing.T) {
	addon := testAddon()
	r, c := newTestReconciler(t)

	gotChart, gotVersion, err := r.getChartVersion(context.Background(), adapter.NewAddonCatalog(c), adapter.NewAddonRepository(helmRepositoryFixture()), adapter.NewAddonRelease(addon), utils.InternalOCIRepository)
	if err == nil {
		t.Fatalf("expected an error when the addon chart does not exist, got version %+v", gotVersion)
	}
	if gotChart != nil || gotVersion != nil {
		t.Fatalf("expected nil chart and version on error, got chart=%v version=%v", gotChart, gotVersion)
	}
}

// stubChartResolver stands in for the registry so a reconcile never leaves the
// process.
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
		NewRelease:   adapter.EmptyAddonRelease,
		Repositories: adapter.NewAddonRepositoryResolver(c),
		Chart:        services.NewChartService(c, scheme, helmv1alpha1.TargetNamespace),
		OCI:          services.NewOCIRepoService(c, scheme, helmv1alpha1.TargetNamespace, resolver),
		Release:      services.NewReleaseService(c, scheme, helmv1alpha1.TargetNamespace),
		Maintenance:  services.NewMaintenanceService(c, scheme, helmv1alpha1.TargetNamespace),
		Claim:        services.NewClaimService(c, c, helmv1alpha1.TargetNamespace),
		Namespaces:   services.NewNamespaceService(c),
		Access:       source.NoAccess{},
		Status:       status.NewManager(c),
	}), c
}

// newApplicationFullReconciler is the sibling of newFullReconciler for the
// application family: the same services, wired to the namespaced adapters. It is
// what lets a test prove that one reconciler serves both families. access is the
// identity manager; nil selects the real one over the same fake client.
func newApplicationFullReconciler(
	t *testing.T,
	access source.AccessManager,
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

	if access == nil {
		access = services.NewAccessService(c, helmv1alpha1.TargetNamespace)
	}

	return New(c, Deps{
		NewRelease:   adapter.EmptyApplicationRelease,
		Repositories: adapter.NewApplicationRepositoryResolver(c),
		Chart:        services.NewChartService(c, scheme, helmv1alpha1.TargetNamespace),
		OCI:          services.NewOCIRepoService(c, scheme, helmv1alpha1.TargetNamespace, nil),
		Release:      services.NewReleaseService(c, scheme, helmv1alpha1.TargetNamespace),
		Maintenance:  services.NewMaintenanceService(c, scheme, helmv1alpha1.TargetNamespace),
		Claim:        source.NoChartClaim{},
		Namespaces:   source.ExistingTargetNamespace{},
		Access:       access,
		Status:       status.NewManager(c),
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

// applicationFixtures builds the objects an application needs to reconcile: the
// namespaced repository it points at and the catalog entry offering its version.
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

// markInternalChartReady stands in for nelm-source-controller: the internal
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
	if len(roles.Items) != 1 || roles.Items[0].Name != services.ApplicationRoleName {
		t.Fatalf("roles = %+v, want only %s", roles.Items, services.ApplicationRoleName)
	}

	var bindings rbacv1.RoleBindingList
	if err := c.List(context.Background(), &bindings, inNamespace); err != nil {
		t.Fatalf("listing role bindings: %v", err)
	}
	if len(bindings.Items) != 1 || bindings.Items[0].Name != names.ServiceAccount {
		t.Fatalf("role bindings = %+v, want only %s", bindings.Items, names.ServiceAccount)
	}
}

// failingAccess is an AccessManager whose identity setup never succeeds.
type failingAccess struct{}

func (failingAccess) EnsureAccess(context.Context, source.Release) error {
	return errors.New("service account is forbidden")
}

func (failingAccess) CleanupAccess(context.Context, source.Release) error { return nil }

// TestReconcileApplicationReportsAccessSetupFailure pins that the pass stops at the
// identity. A release applied without one would run as helm-controller itself,
// which is exactly the privilege the namespaced family exists to avoid, so the
// failure has to be reported instead of worked around.
func TestReconcileApplicationReportsAccessSetupFailure(t *testing.T) {
	app := testApplication()

	r, c := newApplicationFullReconciler(t, failingAccess{}, append(applicationFixtures(), app)...)

	reconcileApplication(t, r, app)

	settled := &helmv1alpha1.HelmApplication{}
	key := types.NamespacedName{Namespace: app.Namespace, Name: app.Name}
	if err := c.Get(context.Background(), key, settled); err != nil {
		t.Fatalf("getting application: %v", err)
	}

	ready := apimeta.FindStatusCondition(settled.Status.Conditions, helmv1alpha1.ConditionTypeReady)
	if ready == nil {
		t.Fatalf("Ready must be reported, conditions: %v", settled.Status.Conditions)
	}
	if ready.Status != metav1.ConditionFalse || ready.Reason != helmv1alpha1.ReasonAccessSetupFailed {
		t.Fatalf("Ready is %s/%s, want False/%s", ready.Status, ready.Reason, helmv1alpha1.ReasonAccessSetupFailed)
	}

	names := adapter.NewApplicationRelease(app).InternalNames()
	releaseKey := client.ObjectKey{Name: names.HelmRelease, Namespace: helmv1alpha1.TargetNamespace}
	if err := c.Get(context.Background(), releaseKey, &helmv2.HelmRelease{}); err == nil {
		t.Fatal("no release must be created for an application whose identity could not be set up")
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
			r.logSourceKindFlip(ctx, adapter.NewAddonRelease(addon), utils.InternalOCIRepository, true)

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

// TestReconcileForcedAddonRecordsCompletion covers the other end of the same pass.
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

	if cond := apimeta.FindStatusCondition(settled.Status.Conditions, helmv1alpha1.ConditionTypeReconciling); cond != nil {
		t.Fatalf("Reconciling must be gone once the forced pass finished, got %+v", cond)
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

// TestReconcileForceAnnotationSkipsUnannotatedAddon pins that an addon carrying
// unrelated annotations is not written on every pass. Guarding on the map instead
// of on the annotation itself sends an empty PATCH each time, which costs a write
// and an update event for every addon in the cluster.
func TestReconcileForceAnnotationSkipsUnannotatedAddon(t *testing.T) {
	addon := testAddon()
	addon.Annotations = map[string]string{"example.io/unrelated": "value"}

	r, c := newForceTestReconciler(t, interceptor.Funcs{}, addon)

	stored := &helmv1alpha1.HelmClusterAddon{}
	key := types.NamespacedName{Name: addon.Name}
	if err := c.Get(context.Background(), key, stored); err != nil {
		t.Fatalf("getting addon: %v", err)
	}
	before := stored.ResourceVersion

	if err := r.reconcileForceAnnotation(context.Background(), key); err != nil {
		t.Fatalf("reconcileForceAnnotation returned %v", err)
	}

	if err := c.Get(context.Background(), key, stored); err != nil {
		t.Fatalf("getting addon: %v", err)
	}
	if stored.ResourceVersion != before {
		t.Fatalf("resourceVersion moved from %s to %s: an addon without the force annotation was written",
			before, stored.ResourceVersion)
	}
	if stored.Annotations["example.io/unrelated"] != "value" {
		t.Fatal("unrelated annotations must be left in place")
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

	if !addon.MaintenanceModeEnabled() || r.deps.Maintenance.IsMaintenanceModeChangeRequired(adapter.NewAddonRelease(addon)) {
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

// TestReconcileLeavingMaintenanceKeepsForceReconcile is the complement. Lifting
// maintenance also returns early, but reconciliation is resuming, so the request is
// about to become actionable and must survive to the pass that can honour it.
func TestReconcileLeavingMaintenanceKeepsForceReconcile(t *testing.T) {
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

	if addon.MaintenanceModeActivated() || !r.deps.Maintenance.IsMaintenanceModeChangeRequired(adapter.NewAddonRelease(addon)) {
		t.Fatal("the fixture must be leaving maintenance, otherwise the test proves nothing")
	}

	reconcileAddon(t, r, addon.Name)

	settled := &helmv1alpha1.HelmClusterAddon{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: addon.Name}, settled); err != nil {
		t.Fatalf("getting addon: %v", err)
	}

	if _, found := settled.Annotations[helmv1alpha1.AnnotationForceReconcile]; !found {
		t.Fatal("the force annotation must survive the pass that lifts maintenance")
	}
}
