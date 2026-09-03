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
	"testing"

	"github.com/werf/3p-fluxcd-pkg/apis/meta"
	sourcev1 "github.com/werf/nelm-source-controller/api/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
	"github.com/deckhouse/operator-helm/internal/index"
	"github.com/deckhouse/operator-helm/internal/utils"
)

func newOCIRepoService(t *testing.T, objects ...client.Object) (*OCIRepoService, client.Client) {
	t.Helper()

	scheme := testScheme(t)
	if err := sourcev1.AddToScheme(scheme); err != nil {
		t.Fatalf("registering source scheme: %v", err)
	}

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objects...).
		WithIndex(&helmv1alpha1.HelmClusterAddon{}, index.AddonRepository, func(obj client.Object) []string {
			addon := obj.(*helmv1alpha1.HelmClusterAddon)

			return []string{addon.Spec.Chart.HelmClusterAddonRepository}
		}).
		Build()

	return NewOCIRepoService(c, scheme, testNamespace), c
}

func internalOCIRepository(addonName string) *sourcev1.OCIRepository {
	return &sourcev1.OCIRepository{
		ObjectMeta: metav1.ObjectMeta{
			Name:      utils.GetInternalOCIRepositoryName(addonName),
			Namespace: testNamespace,
		},
	}
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

func ociTestRepository() *helmv1alpha1.HelmClusterAddonRepository {
	return &helmv1alpha1.HelmClusterAddonRepository{
		ObjectMeta: metav1.ObjectMeta{Name: "example", Generation: 1},
		Spec:       helmv1alpha1.HelmClusterAddonRepositorySpec{URL: "oci://example.invalid/podinfo"},
	}
}

func TestEnsureInternalOCIRepositoryUsesRecordedMediaType(t *testing.T) {
	addon, repo := testAddon(), ociTestRepository()
	service, c := newOCIRepoService(t, addon, repo)

	version := &helmv1alpha1.HelmClusterAddonChartVersion{
		Version:   "6.7.1",
		MediaType: "application/tar+gzip",
	}

	service.EnsureInternalOCIRepository(context.Background(), addon, repo, version)

	ociRepo := &sourcev1.OCIRepository{}
	key := client.ObjectKey{Name: utils.GetInternalOCIRepositoryName(addon.Name), Namespace: testNamespace}
	if err := c.Get(context.Background(), key, ociRepo); err != nil {
		t.Fatalf("oci repository was not created: %v", err)
	}

	if ociRepo.Spec.LayerSelector == nil {
		t.Fatal("layer selector must be set")
	}
	if ociRepo.Spec.LayerSelector.MediaType != "application/tar+gzip" {
		t.Fatalf("layer media type is %q, want the recorded legacy one", ociRepo.Spec.LayerSelector.MediaType)
	}
}

func TestEnsureInternalOCIRepositoryReportsRemovedVersion(t *testing.T) {
	addon, repo := testAddon(), ociTestRepository()
	service, _ := newOCIRepoService(t, addon, repo)

	version := &helmv1alpha1.HelmClusterAddonChartVersion{
		Version:           "6.7.1",
		MediaType:         "application/tar+gzip",
		UnavailableReason: helmv1alpha1.UnavailableReasonRemovedFromRepository,
	}

	result := service.EnsureInternalOCIRepository(context.Background(), addon, repo, version)

	if result.Status.Reason != helmv1alpha1.ReasonChartVersionRemoved {
		t.Fatalf("reason is %q, want %q", result.Status.Reason, helmv1alpha1.ReasonChartVersionRemoved)
	}
	if result.Status.Message == "" {
		t.Fatal("a removed version must be explained in the message")
	}
}

// TestEnsureInternalOCIRepositoryDoesNotRelabelReadyChildOnRemovedVersion covers the
// complement of TestEnsureInternalOCIRepositoryReportsRemovedVersion: a version can
// still carry UnavailableReasonRemovedFromRepository after the tag reappears (the
// marker is only dropped on the next synchronization), and by then the child
// OCIRepository may already be healthy again. The override must not fire in that
// case - a ready addon must not be relabeled with a failure reason.
//
// The internal object is seeded with the exact spec and labels applyOCIRepositorySpec
// writes (same trick as TestEnsureInternalHelmRepositoryStalledPrecedesReady in
// helm_repo_service_test.go): that makes CreateOrPatch a no-op, so the object's
// generation stays at 1 and the seeded Ready condition (ObservedGeneration: 1) counts
// as observed.
func TestEnsureInternalOCIRepositoryDoesNotRelabelReadyChildOnRemovedVersion(t *testing.T) {
	addon, repo := testAddon(), ociTestRepository()

	version := &helmv1alpha1.HelmClusterAddonChartVersion{
		Version:           "6.7.1",
		MediaType:         "application/tar+gzip",
		UnavailableReason: helmv1alpha1.UnavailableReasonRemovedFromRepository,
	}

	internal := &sourcev1.OCIRepository{
		ObjectMeta: metav1.ObjectMeta{
			Name:       utils.GetInternalOCIRepositoryName(addon.Name),
			Namespace:  testNamespace,
			Generation: 1,
			Labels: map[string]string{
				helmv1alpha1.LabelManagedBy:                  helmv1alpha1.LabelManagedByValue,
				helmv1alpha1.HelmClusterAddonLabelSourceName: addon.Name,
			},
		},
		Spec: sourcev1.OCIRepositorySpec{
			URL:       repo.Spec.URL,
			Reference: &sourcev1.OCIRepositoryRef{Tag: addon.Spec.Chart.Version},
			Interval:  metav1.Duration{Duration: InternalRepositoryInterval},
			LayerSelector: &sourcev1.OCILayerSelector{
				MediaType: version.MediaType,
				Operation: "copy",
			},
		},
		Status: sourcev1.OCIRepositoryStatus{
			Conditions: []metav1.Condition{
				{
					Type:               helmv1alpha1.ConditionTypeReady,
					Status:             metav1.ConditionTrue,
					Reason:             "Succeeded",
					Message:            "stored artifact for revision 6.7.1",
					ObservedGeneration: 1,
					LastTransitionTime: metav1.Now(),
				},
			},
		},
	}

	service, _ := newOCIRepoService(t, addon, repo, internal)

	result := service.EnsureInternalOCIRepository(context.Background(), addon, repo, version)

	if result.Status.Status != metav1.ConditionTrue {
		t.Fatalf("expected the ready child's status to be mirrored as True, got %v", result.Status.Status)
	}
	if result.Status.Reason == helmv1alpha1.ReasonChartVersionRemoved {
		t.Fatalf("a ready child must not be relabeled with %q", helmv1alpha1.ReasonChartVersionRemoved)
	}
	if result.Status.Reason != "Succeeded" {
		t.Fatalf("reason is %q, want the child's own %q untouched", result.Status.Reason, "Succeeded")
	}
	if result.Status.Message != "stored artifact for revision 6.7.1" {
		t.Fatalf("message is %q, want the child's own message untouched", result.Status.Message)
	}
}

// TestEnsureInternalOCIRepositoryForcesReconcileFromAddon covers the force
// reconcile annotation applied to the HelmClusterAddon itself: it must reach the
// internal OCIRepository, otherwise the source is never re-pulled and only the
// HelmRelease is nudged.
func TestEnsureInternalOCIRepositoryForcesReconcileFromAddon(t *testing.T) {
	addon, repo := testAddon(), ociTestRepository()
	addon.Annotations = map[string]string{helmv1alpha1.AnnotationForceReconcile: "2026-01-01T00:00:00Z"}
	service, c := newOCIRepoService(t, addon, repo)

	version := &helmv1alpha1.HelmClusterAddonChartVersion{
		Version:   "6.7.1",
		MediaType: "application/tar+gzip",
	}

	service.EnsureInternalOCIRepository(context.Background(), addon, repo, version)

	ociRepo := &sourcev1.OCIRepository{}
	key := client.ObjectKey{Name: utils.GetInternalOCIRepositoryName(addon.Name), Namespace: testNamespace}
	if err := c.Get(context.Background(), key, ociRepo); err != nil {
		t.Fatalf("oci repository was not created: %v", err)
	}

	if ociRepo.Annotations[meta.ReconcileRequestAnnotation] == "" {
		t.Errorf("%s must be stamped on the oci repository", meta.ReconcileRequestAnnotation)
	}
	if ociRepo.Annotations[meta.ForceRequestAnnotation] == "" {
		t.Errorf("%s must be stamped on the oci repository", meta.ForceRequestAnnotation)
	}
}

// TestEnsureInternalOCIRepositoryDoesNotForceReconcileWithoutAnnotation is the
// complement: an unannotated addon must not stamp a fresh timestamp on every
// pass, which would make the source controller re-reconcile continuously.
func TestEnsureInternalOCIRepositoryDoesNotForceReconcileWithoutAnnotation(t *testing.T) {
	addon, repo := testAddon(), ociTestRepository()
	service, c := newOCIRepoService(t, addon, repo)

	version := &helmv1alpha1.HelmClusterAddonChartVersion{
		Version:   "6.7.1",
		MediaType: "application/tar+gzip",
	}

	service.EnsureInternalOCIRepository(context.Background(), addon, repo, version)

	ociRepo := &sourcev1.OCIRepository{}
	key := client.ObjectKey{Name: utils.GetInternalOCIRepositoryName(addon.Name), Namespace: testNamespace}
	if err := c.Get(context.Background(), key, ociRepo); err != nil {
		t.Fatalf("oci repository was not created: %v", err)
	}

	if _, found := ociRepo.Annotations[meta.ReconcileRequestAnnotation]; found {
		t.Errorf("%s must not be stamped without a force request", meta.ReconcileRequestAnnotation)
	}
}

// TestForceReconcileInternalRepositoriesStampsOnlyItsOwnAddons covers the force
// reconcile annotation applied to an oci:// HelmClusterAddonRepository: unlike the
// helm:// path, where the internal HelmRepository re-indexes and the HelmCharts
// follow, an OCI repository has no intermediate source object, so the request must
// be pushed onto the internal OCIRepository of each addon that references it - and
// only of those addons.
func TestForceReconcileInternalRepositoriesStampsOnlyItsOwnAddons(t *testing.T) {
	addon := testAddon()
	foreign := testAddon()
	foreign.Name = "foreign"
	foreign.Spec.Chart.HelmClusterAddonRepository = "another"

	service, c := newOCIRepoService(t,
		addon, foreign,
		internalOCIRepository(addon.Name), internalOCIRepository(foreign.Name),
	)

	if err := service.ForceReconcileInternalRepositories(context.Background(), "example"); err != nil {
		t.Fatalf("forcing internal repositories: %v", err)
	}

	ociRepo := &sourcev1.OCIRepository{}
	key := client.ObjectKey{Name: utils.GetInternalOCIRepositoryName(addon.Name), Namespace: testNamespace}
	if err := c.Get(context.Background(), key, ociRepo); err != nil {
		t.Fatalf("getting oci repository: %v", err)
	}
	if ociRepo.Annotations[meta.ReconcileRequestAnnotation] == "" {
		t.Errorf("%s must be stamped on the oci repository of the addon", meta.ReconcileRequestAnnotation)
	}
	if ociRepo.Annotations[meta.ForceRequestAnnotation] == "" {
		t.Errorf("%s must be stamped on the oci repository of the addon", meta.ForceRequestAnnotation)
	}

	foreignRepo := &sourcev1.OCIRepository{}
	key = client.ObjectKey{Name: utils.GetInternalOCIRepositoryName(foreign.Name), Namespace: testNamespace}
	if err := c.Get(context.Background(), key, foreignRepo); err != nil {
		t.Fatalf("getting foreign oci repository: %v", err)
	}
	if _, found := foreignRepo.Annotations[meta.ReconcileRequestAnnotation]; found {
		t.Errorf("%s must not be stamped on an addon of another repository", meta.ReconcileRequestAnnotation)
	}
}

// TestForceReconcileInternalRepositoriesToleratesMissingSource covers the addon
// that has no internal OCIRepository yet - it has just been created, or it never
// reached the point of building one. A force on the repository must not fail
// because of it, otherwise the request is retried forever.
func TestForceReconcileInternalRepositoriesToleratesMissingSource(t *testing.T) {
	addon := testAddon()
	service, _ := newOCIRepoService(t, addon)

	if err := service.ForceReconcileInternalRepositories(context.Background(), "example"); err != nil {
		t.Fatalf("a missing internal oci repository must not fail the force request: %v", err)
	}
}
