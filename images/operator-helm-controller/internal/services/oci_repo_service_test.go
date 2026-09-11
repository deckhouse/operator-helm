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

	"github.com/werf/3p-fluxcd-pkg/apis/meta"
	sourcev1 "github.com/werf/nelm-source-controller/api/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
	"github.com/deckhouse/operator-helm/internal/adapter"
	repoclient "github.com/deckhouse/operator-helm/internal/client/repository"
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

	return NewOCIRepoService(c, scheme, testNamespace, nil), c
}

// countingResolver records how many times the registry was asked, which is the whole
// point of the cache: in the steady state the answer must be zero.
type countingResolver struct {
	mediaType string
	err       error
	calls     int
	refs      []string
}

func (r *countingResolver) ResolveChartArtifact(_ context.Context, ref string, _ *repoclient.RepoConfig) (string, error) {
	r.calls++
	r.refs = append(r.refs, ref)

	return r.mediaType, r.err
}

func newOCIRepoServiceWithResolver(
	t *testing.T,
	resolver repoclient.ChartResolverInterface,
	objects ...client.Object,
) (*OCIRepoService, client.Client) {
	t.Helper()

	service, c := newOCIRepoService(t, objects...)
	service.resolver = resolver

	return service, c
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
		Spec:       helmv1alpha1.RepositorySpec{URL: "oci://example.invalid/podinfo"},
	}
}

// ociSource resolves the source the way the reconciler does, so the tests exercise
// the real mapping instead of a hand-built one.
func ociSource(t *testing.T, repo *helmv1alpha1.HelmClusterAddonRepository, version *helmv1alpha1.ChartVersion) utils.ChartSource {
	t.Helper()

	source, err := utils.ResolveChartSource(repo.Spec.URL, version)
	if err != nil {
		t.Fatalf("resolving chart source: %v", err)
	}

	return source
}

// hybridTestRepository is a helm repository: its own url is served over https, and
// only the index entry of a version points at a registry.
func hybridTestRepository() *helmv1alpha1.HelmClusterAddonRepository {
	return &helmv1alpha1.HelmClusterAddonRepository{
		ObjectMeta: metav1.ObjectMeta{Name: "example", Generation: 1},
		Spec: helmv1alpha1.RepositorySpec{
			URL:                "https://charts.example.invalid/stable",
			Auth:               &helmv1alpha1.RepositoryAuth{Username: "u", Password: "p"},
			CACertificate:      "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----",
			InsecureSkipVerify: true,
		},
	}
}

func TestEnsureInternalOCIRepositoryUsesRecordedMediaType(t *testing.T) {
	addon, repo := testAddon(), ociTestRepository()
	service, c := newOCIRepoService(t, addon, repo)

	version := &helmv1alpha1.ChartVersion{
		Version:   "6.7.1",
		MediaType: "application/tar+gzip",
	}

	service.EnsureInternalOCIRepository(context.Background(), adapter.NewAddonRelease(addon), adapter.NewAddonRepository(repo), ociSource(t, repo, version), version)

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

	version := &helmv1alpha1.ChartVersion{
		Version:           "6.7.1",
		MediaType:         "application/tar+gzip",
		UnavailableReason: helmv1alpha1.UnavailableReasonRemovedFromRepository,
	}

	result := service.EnsureInternalOCIRepository(context.Background(), adapter.NewAddonRelease(addon), adapter.NewAddonRepository(repo), ociSource(t, repo, version), version)

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

	version := &helmv1alpha1.ChartVersion{
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

	result := service.EnsureInternalOCIRepository(context.Background(), adapter.NewAddonRelease(addon), adapter.NewAddonRepository(repo), ociSource(t, repo, version), version)

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

	version := &helmv1alpha1.ChartVersion{
		Version:   "6.7.1",
		MediaType: "application/tar+gzip",
	}

	service.EnsureInternalOCIRepository(context.Background(), adapter.NewAddonRelease(addon), adapter.NewAddonRepository(repo), ociSource(t, repo, version), version)

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

	version := &helmv1alpha1.ChartVersion{
		Version:   "6.7.1",
		MediaType: "application/tar+gzip",
	}

	service.EnsureInternalOCIRepository(context.Background(), adapter.NewAddonRelease(addon), adapter.NewAddonRepository(repo), ociSource(t, repo, version), version)

	ociRepo := &sourcev1.OCIRepository{}
	key := client.ObjectKey{Name: utils.GetInternalOCIRepositoryName(addon.Name), Namespace: testNamespace}
	if err := c.Get(context.Background(), key, ociRepo); err != nil {
		t.Fatalf("oci repository was not created: %v", err)
	}

	if _, found := ociRepo.Annotations[meta.ReconcileRequestAnnotation]; found {
		t.Errorf("%s must not be stamped without a force request", meta.ReconcileRequestAnnotation)
	}
}

// TestEnsureInternalOCIRepositoryAddressesTheIndexReference pins that the artifact
// address comes from the version, not from the repository: for a hybrid version the
// registry is a different host entirely.
func TestEnsureInternalOCIRepositoryAddressesTheIndexReference(t *testing.T) {
	addon, repo := testAddon(), hybridTestRepository()
	resolver := &countingResolver{mediaType: "application/vnd.cncf.helm.chart.content.v1.tar+gzip"}
	service, c := newOCIRepoServiceWithResolver(t, resolver, addon, repo)

	version := &helmv1alpha1.ChartVersion{
		Version: "6.7.1",
		OCIRef:  "oci://registry.example.com/charts/podinfo:6.7.1",
	}

	service.EnsureInternalOCIRepository(
		context.Background(), adapter.NewAddonRelease(addon), adapter.NewAddonRepository(repo), ociSource(t, repo, version), version,
	)

	ociRepo := &sourcev1.OCIRepository{}
	key := client.ObjectKey{Name: utils.GetInternalOCIRepositoryName(addon.Name), Namespace: testNamespace}
	if err := c.Get(context.Background(), key, ociRepo); err != nil {
		t.Fatalf("oci repository was not created: %v", err)
	}

	if ociRepo.Spec.URL != "oci://registry.example.com/charts/podinfo" {
		t.Fatalf("url = %q, want the address from the index reference", ociRepo.Spec.URL)
	}
	if ociRepo.Spec.Reference == nil || ociRepo.Spec.Reference.Tag != "6.7.1" {
		t.Fatalf("reference = %+v, want tag 6.7.1", ociRepo.Spec.Reference)
	}

	// Only public registries are supported, and the repository's transport settings
	// describe its own host, not a third-party one.
	if ociRepo.Spec.SecretRef != nil {
		t.Errorf("credentials must not be sent to a registry the index merely names")
	}
	if ociRepo.Spec.CertSecretRef != nil {
		t.Errorf("the repository CA does not apply to a different host")
	}
	if ociRepo.Spec.Insecure {
		t.Errorf("insecure must not be carried to a different host")
	}
}

// TestEnsureInternalOCIRepositoryCarriesTLSOnTheSameHost covers the one case where
// the repository's transport settings do apply: an internal host serving both the
// index and the registry. The auth secret still cannot be referenced — a helm
// repository stores it as an Opaque username/password secret, and OCIRepository
// accepts only dockerconfigjson.
func TestEnsureInternalOCIRepositoryCarriesTLSOnTheSameHost(t *testing.T) {
	addon, repo := testAddon(), hybridTestRepository()
	resolver := &countingResolver{mediaType: "application/vnd.cncf.helm.chart.content.v1.tar+gzip"}
	service, c := newOCIRepoServiceWithResolver(t, resolver, addon, repo)

	version := &helmv1alpha1.ChartVersion{
		Version: "6.7.1",
		OCIRef:  "oci://charts.example.invalid/charts/podinfo:6.7.1",
	}

	service.EnsureInternalOCIRepository(
		context.Background(), adapter.NewAddonRelease(addon), adapter.NewAddonRepository(repo), ociSource(t, repo, version), version,
	)

	ociRepo := &sourcev1.OCIRepository{}
	key := client.ObjectKey{Name: utils.GetInternalOCIRepositoryName(addon.Name), Namespace: testNamespace}
	if err := c.Get(context.Background(), key, ociRepo); err != nil {
		t.Fatalf("oci repository was not created: %v", err)
	}

	if ociRepo.Spec.CertSecretRef == nil {
		t.Error("the repository CA applies to its own host")
	}
	if !ociRepo.Spec.Insecure {
		t.Error("insecure applies to the repository's own host")
	}
	if ociRepo.Spec.SecretRef != nil {
		t.Error("a helm repository's Opaque auth secret must not be referenced by an OCIRepository")
	}
}

// TestEnsureInternalOCIRepositoryKeepsOCIRepositoryCredentials is the regression
// guard for the existing behaviour: for an oci:// repository the artifact host is the
// repository host, so its auth and CA still apply.
func TestEnsureInternalOCIRepositoryKeepsOCIRepositoryCredentials(t *testing.T) {
	addon, repo := testAddon(), ociTestRepository()
	repo.Spec.Auth = &helmv1alpha1.RepositoryAuth{Username: "u", Password: "p"}
	repo.Spec.CACertificate = "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----"

	service, c := newOCIRepoService(t, addon, repo)

	version := &helmv1alpha1.ChartVersion{
		Version:   "6.7.1",
		MediaType: "application/tar+gzip",
	}

	service.EnsureInternalOCIRepository(
		context.Background(), adapter.NewAddonRelease(addon), adapter.NewAddonRepository(repo), ociSource(t, repo, version), version,
	)

	ociRepo := &sourcev1.OCIRepository{}
	key := client.ObjectKey{Name: utils.GetInternalOCIRepositoryName(addon.Name), Namespace: testNamespace}
	if err := c.Get(context.Background(), key, ociRepo); err != nil {
		t.Fatalf("oci repository was not created: %v", err)
	}

	if ociRepo.Spec.URL != repo.Spec.URL {
		t.Fatalf("url = %q, want %q", ociRepo.Spec.URL, repo.Spec.URL)
	}
	if ociRepo.Spec.SecretRef == nil {
		t.Error("an oci repository's own credentials must still be referenced")
	}
	if ociRepo.Spec.CertSecretRef == nil {
		t.Error("an oci repository's own CA must still be referenced")
	}
}

func TestEnsureInternalOCIRepositoryProbesHybridVersion(t *testing.T) {
	addon, repo := testAddon(), hybridTestRepository()
	resolver := &countingResolver{mediaType: "application/vnd.cncf.helm.chart.content.v1.tar+gzip"}
	service, c := newOCIRepoServiceWithResolver(t, resolver, addon, repo)

	version := &helmv1alpha1.ChartVersion{
		Version: "6.7.1",
		OCIRef:  "oci://registry.example.com/charts/podinfo:6.7.1",
	}

	service.EnsureInternalOCIRepository(
		context.Background(), adapter.NewAddonRelease(addon), adapter.NewAddonRepository(repo), ociSource(t, repo, version), version,
	)

	if resolver.calls != 1 {
		t.Fatalf("resolver calls = %d, want 1", resolver.calls)
	}
	if resolver.refs[0] != "oci://registry.example.com/charts/podinfo:6.7.1" {
		t.Fatalf("probed %q, want the full reference", resolver.refs[0])
	}

	ociRepo := &sourcev1.OCIRepository{}
	key := client.ObjectKey{Name: utils.GetInternalOCIRepositoryName(addon.Name), Namespace: testNamespace}
	if err := c.Get(context.Background(), key, ociRepo); err != nil {
		t.Fatalf("oci repository was not created: %v", err)
	}
	if ociRepo.Spec.LayerSelector == nil || ociRepo.Spec.LayerSelector.MediaType != resolver.mediaType {
		t.Fatalf("layer selector = %+v, want the probed media type", ociRepo.Spec.LayerSelector)
	}
}

// TestEnsureInternalOCIRepositoryReusesTheInternalObjectAsCache is the steady state:
// the internal object already addresses this artifact and carries the verdict, so the
// registry must not be asked again on every pass.
func TestEnsureInternalOCIRepositoryReusesTheInternalObjectAsCache(t *testing.T) {
	addon, repo := testAddon(), hybridTestRepository()
	resolver := &countingResolver{mediaType: "application/vnd.cncf.helm.chart.content.v1.tar+gzip"}
	service, _ := newOCIRepoServiceWithResolver(t, resolver, addon, repo)

	version := &helmv1alpha1.ChartVersion{
		Version: "6.7.1",
		OCIRef:  "oci://registry.example.com/charts/podinfo:6.7.1",
	}
	source := ociSource(t, repo, version)

	service.EnsureInternalOCIRepository(context.Background(), adapter.NewAddonRelease(addon), adapter.NewAddonRepository(repo), source, version)
	service.EnsureInternalOCIRepository(context.Background(), adapter.NewAddonRelease(addon), adapter.NewAddonRepository(repo), source, version)

	if resolver.calls != 1 {
		t.Fatalf("resolver calls = %d, want 1: the internal object is the cache", resolver.calls)
	}
}

// TestEnsureInternalOCIRepositoryReprobesChangedReference covers a repository that
// re-published the same version somewhere else: the cached verdict describes a
// different artifact and must not be reused.
func TestEnsureInternalOCIRepositoryReprobesChangedReference(t *testing.T) {
	addon, repo := testAddon(), hybridTestRepository()
	resolver := &countingResolver{mediaType: "application/vnd.cncf.helm.chart.content.v1.tar+gzip"}
	service, _ := newOCIRepoServiceWithResolver(t, resolver, addon, repo)

	first := &helmv1alpha1.ChartVersion{
		Version: "6.7.1",
		OCIRef:  "oci://registry.example.com/charts/podinfo:6.7.1",
	}
	service.EnsureInternalOCIRepository(context.Background(), adapter.NewAddonRelease(addon), adapter.NewAddonRepository(repo), ociSource(t, repo, first), first)

	second := &helmv1alpha1.ChartVersion{
		Version: "6.7.1",
		OCIRef:  "oci://mirror.example.com/charts/podinfo:6.7.1",
	}
	service.EnsureInternalOCIRepository(context.Background(), adapter.NewAddonRelease(addon), adapter.NewAddonRepository(repo), ociSource(t, repo, second), second)

	if resolver.calls != 2 {
		t.Fatalf("resolver calls = %d, want 2: the reference changed", resolver.calls)
	}
}

// TestEnsureInternalOCIRepositoryForceBypassesCache: a force request means
// "re-examine", the same thing it means for the repository catalog.
func TestEnsureInternalOCIRepositoryForceBypassesCache(t *testing.T) {
	addon, repo := testAddon(), hybridTestRepository()
	resolver := &countingResolver{mediaType: "application/vnd.cncf.helm.chart.content.v1.tar+gzip"}
	service, _ := newOCIRepoServiceWithResolver(t, resolver, addon, repo)

	version := &helmv1alpha1.ChartVersion{
		Version: "6.7.1",
		OCIRef:  "oci://registry.example.com/charts/podinfo:6.7.1",
	}
	source := ociSource(t, repo, version)

	service.EnsureInternalOCIRepository(context.Background(), adapter.NewAddonRelease(addon), adapter.NewAddonRepository(repo), source, version)

	addon.Annotations = map[string]string{helmv1alpha1.AnnotationForceReconcile: "2026-01-01T00:00:00Z"}
	service.EnsureInternalOCIRepository(context.Background(), adapter.NewAddonRelease(addon), adapter.NewAddonRepository(repo), source, version)

	if resolver.calls != 2 {
		t.Fatalf("resolver calls = %d, want 2: a force request re-examines the artifact", resolver.calls)
	}
}

// TestEnsureInternalOCIRepositoryNeverProbesRecordedMediaType: an oci:// repository's
// version carries the verdict from the catalog, so the registry is not asked at all.
func TestEnsureInternalOCIRepositoryNeverProbesRecordedMediaType(t *testing.T) {
	addon, repo := testAddon(), ociTestRepository()
	resolver := &countingResolver{mediaType: "should-not-be-used"}
	service, _ := newOCIRepoServiceWithResolver(t, resolver, addon, repo)

	version := &helmv1alpha1.ChartVersion{
		Version:   "6.7.1",
		MediaType: "application/tar+gzip",
	}

	service.EnsureInternalOCIRepository(
		context.Background(), adapter.NewAddonRelease(addon), adapter.NewAddonRepository(repo), ociSource(t, repo, version), version,
	)

	if resolver.calls != 0 {
		t.Fatalf("resolver calls = %d, want 0", resolver.calls)
	}
}

func TestEnsureInternalOCIRepositoryReportsTerminalProbeFailure(t *testing.T) {
	addon, repo := testAddon(), hybridTestRepository()
	resolver := &countingResolver{err: &repoclient.TerminalError{
		Reason:  helmv1alpha1.ReasonUnsupportedChartArtifact,
		Message: "config media type is not a helm chart config",
	}}
	service, c := newOCIRepoServiceWithResolver(t, resolver, addon, repo)

	version := &helmv1alpha1.ChartVersion{
		Version: "6.7.1",
		OCIRef:  "oci://registry.example.com/charts/podinfo:6.7.1",
	}

	result := service.EnsureInternalOCIRepository(
		context.Background(), adapter.NewAddonRelease(addon), adapter.NewAddonRepository(repo), ociSource(t, repo, version), version,
	)

	if result.Status.Reason != helmv1alpha1.ReasonUnsupportedChartArtifact {
		t.Fatalf("reason = %q, want %q", result.Status.Reason, helmv1alpha1.ReasonUnsupportedChartArtifact)
	}
	if result.RequeueAfter != 0 {
		t.Fatalf("requeue = %v, want none: the artifact will not become a chart on its own", result.RequeueAfter)
	}

	// Nothing must be created from a verdict that says the artifact is unusable: an
	// OCIRepository with an empty layer selector would make the source controller
	// guess the layer.
	ociRepo := &sourcev1.OCIRepository{}
	key := client.ObjectKey{Name: utils.GetInternalOCIRepositoryName(addon.Name), Namespace: testNamespace}
	if err := c.Get(context.Background(), key, ociRepo); err == nil {
		t.Fatal("no internal oci repository must be created for an unusable artifact")
	}
}

func TestEnsureInternalOCIRepositoryRequeuesRetriableProbeFailure(t *testing.T) {
	addon, repo := testAddon(), hybridTestRepository()
	resolver := &countingResolver{err: errors.New("429 Too Many Requests")}
	service, _ := newOCIRepoServiceWithResolver(t, resolver, addon, repo)

	version := &helmv1alpha1.ChartVersion{
		Version: "6.7.1",
		OCIRef:  "oci://registry.example.com/charts/podinfo:6.7.1",
	}

	result := service.EnsureInternalOCIRepository(
		context.Background(), adapter.NewAddonRelease(addon), adapter.NewAddonRepository(repo), ociSource(t, repo, version), version,
	)

	if result.Status.Status != metav1.ConditionFalse {
		t.Fatalf("status = %q, want False", result.Status.Status)
	}
	if result.RequeueAfter != chartArtifactProbeRequeueInterval {
		t.Fatalf("requeue = %v, want %v", result.RequeueAfter, chartArtifactProbeRequeueInterval)
	}
}
