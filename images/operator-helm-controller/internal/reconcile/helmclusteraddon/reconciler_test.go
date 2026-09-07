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

package helmclusteraddon

import (
	"context"
	"strings"
	"testing"
	"time"

	helmv2 "github.com/werf/3p-helm-controller/api/v2"
	sourcev1 "github.com/werf/nelm-source-controller/api/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/deckhouse/operator-helm/api/naming"
	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
	"github.com/deckhouse/operator-helm/internal/manager/status"
	"github.com/deckhouse/operator-helm/internal/services"
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

func newTestReconciler(t *testing.T, objects ...client.Object) *Reconciler {
	t.Helper()

	scheme := testScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()

	return &Reconciler{Client: c}
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

func addonChartFixture(repoName, chartName string, versions ...helmv1alpha1.HelmClusterAddonChartVersion) *helmv1alpha1.HelmClusterAddonChart {
	return &helmv1alpha1.HelmClusterAddonChart{
		ObjectMeta: metav1.ObjectMeta{
			Name: naming.HelmClusterAddonChartName(repoName, chartName),
		},
		Status: helmv1alpha1.HelmClusterAddonChartStatus{Versions: versions},
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
		version        helmv1alpha1.HelmClusterAddonChartVersion
		repoType       utils.InternalRepositoryType
		wantErr        bool
		wantErrContain string
	}{
		{
			name: "oci version with a media type passes",
			version: helmv1alpha1.HelmClusterAddonChartVersion{
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
			version: helmv1alpha1.HelmClusterAddonChartVersion{
				Version:           "6.7.1",
				MediaType:         "application/tar+gzip",
				UnavailableReason: helmv1alpha1.UnavailableReasonRemovedFromRepository,
			},
			repoType: utils.InternalOCIRepository,
		},
		{
			name: "oci version stuck resolving is rejected with reason and message",
			version: helmv1alpha1.HelmClusterAddonChartVersion{
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
			version: helmv1alpha1.HelmClusterAddonChartVersion{
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
			version: helmv1alpha1.HelmClusterAddonChartVersion{
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
			version: helmv1alpha1.HelmClusterAddonChartVersion{
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
			version: helmv1alpha1.HelmClusterAddonChartVersion{
				Version: "6.7.1",
			},
			repoType:       utils.InternalOCIRepository,
			wantErr:        true,
			wantErrContain: "has not resolved it yet",
		},
		{
			name:           "a version the addon does not reference is rejected",
			version:        helmv1alpha1.HelmClusterAddonChartVersion{Version: "9.9.9"},
			repoType:       utils.InternalOCIRepository,
			wantErr:        true,
			wantErrContain: `does not have version "6.7.1"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			chart := addonChartFixture(
				addon.Spec.Chart.HelmClusterAddonRepository, addon.Spec.Chart.HelmClusterAddonChartName, tt.version,
			)
			r := newTestReconciler(t, chart)

			gotChart, gotVersion, err := r.getHelmClusterAddonChart(context.Background(), addon, tt.repoType)

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
	r := newTestReconciler(t)

	gotChart, gotVersion, err := r.getHelmClusterAddonChart(context.Background(), addon, utils.InternalOCIRepository)
	if err == nil {
		t.Fatalf("expected an error when the addon chart does not exist, got version %+v", gotVersion)
	}
	if gotChart != nil || gotVersion != nil {
		t.Fatalf("expected nil chart and version on error, got chart=%v version=%v", gotChart, gotVersion)
	}
}

// newForceTestReconciler builds a reconciler with the full service set, so a test
// can drive a complete pass rather than a single helper.
func newForceTestReconciler(
	t *testing.T,
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

	return New(
		c,
		services.NewChartService(c, scheme, helmv1alpha1.TargetNamespace),
		services.NewOCIRepoService(c, scheme, helmv1alpha1.TargetNamespace),
		services.NewReleaseService(c, scheme, helmv1alpha1.TargetNamespace),
		services.NewMaintenanceService(c, scheme, helmv1alpha1.TargetNamespace),
		services.NewClaimService(c, c, helmv1alpha1.TargetNamespace),
		status.NewManager(c),
	), c
}

func ociRepositoryFixture() *helmv1alpha1.HelmClusterAddonRepository {
	return &helmv1alpha1.HelmClusterAddonRepository{
		ObjectMeta: metav1.ObjectMeta{Name: "example", Generation: 1},
		Spec:       helmv1alpha1.HelmClusterAddonRepositorySpec{URL: "oci://ghcr.io/example/podinfo"},
	}
}

func forceTestFixtures() []client.Object {
	return []client.Object{
		ociRepositoryFixture(),
		addonChartFixture("example", "podinfo", helmv1alpha1.HelmClusterAddonChartVersion{
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
				observed.Status.Conditions, helmv1alpha1.ConditionTypeReconciling)
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

	if !addon.MaintenanceModeEnabled() || r.maintenanceService.IsMaintenanceModeChangeRequired(addon) {
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

	if addon.MaintenanceModeActivated() || !r.maintenanceService.IsMaintenanceModeChangeRequired(addon) {
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
