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

package internalobj

import (
	"context"
	"errors"
	"testing"

	"github.com/fluxcd/pkg/apis/meta"
	sourcev1 "github.com/fluxcd/source-controller/api/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
	"github.com/deckhouse/operator-helm/internal/adapter"
	"github.com/deckhouse/operator-helm/internal/utils"
)

func sourceScheme(t *testing.T) *runtime.Scheme {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("registering client-go scheme: %v", err)
	}
	if err := helmv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("registering helm scheme: %v", err)
	}
	if err := sourcev1.AddToScheme(scheme); err != nil {
		t.Fatalf("registering source scheme: %v", err)
	}

	return scheme
}

func newHelmRepositories(t *testing.T, objects ...client.Object) *HelmRepositories {
	t.Helper()

	scheme := sourceScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()

	return NewHelmRepositories(c, testNamespace)
}

func testRepository() *helmv1alpha1.HelmClusterAddonRepository {
	return &helmv1alpha1.HelmClusterAddonRepository{
		ObjectMeta: metav1.ObjectMeta{Name: "example", Generation: 1},
		Spec:       helmv1alpha1.RepositorySpec{URL: "https://example.invalid/charts"},
	}
}

func TestHelmRepositoriesEnsureReportsNotObservedAsNotReady(t *testing.T) {
	repo := testRepository()
	service := newHelmRepositories(t, repo)

	state, err := service.Ensure(context.Background(), adapter.NewAddonRepository(repo))
	if err != nil {
		t.Fatalf("Ensure returned %v", err)
	}
	if !state.Present {
		t.Fatal("helm repositories must report an internal object")
	}
	if state.Ready {
		t.Fatal("a freshly created internal object must not be reported ready")
	}
}

func TestHelmRepositoriesEnsureMirrorsConditions(t *testing.T) {
	repo := testRepository()
	// The spec and labels must already match what applyHelmRepositorySpec writes:
	// otherwise CreateOrPatch mutates the object, the fake client bumps its
	// generation, and the Ready condition stops counting as observed.
	internal := &sourcev1.HelmRepository{
		ObjectMeta: metav1.ObjectMeta{
			Name:      utils.GetInternalHelmRepositoryName(repo.Name),
			Namespace: testNamespace,
			Labels: map[string]string{
				helmv1alpha1.LabelManagedBy:                            helmv1alpha1.LabelManagedByValue,
				helmv1alpha1.HelmClusterAddonRepositoryLabelSourceName: repo.Name,
			},
		},
		Spec: sourcev1.HelmRepositorySpec{
			URL:      repo.Spec.URL,
			Interval: metav1.Duration{Duration: InternalRepositoryInterval},
		},
		Status: sourcev1.HelmRepositoryStatus{
			Conditions: []metav1.Condition{
				{
					Type:               helmv1alpha1.ConditionTypeReady,
					Status:             metav1.ConditionFalse,
					Reason:             "FetchFailed",
					Message:            "failed to fetch index",
					LastTransitionTime: metav1.Now(),
				},
			},
		},
	}

	service := newHelmRepositories(t, repo, internal)

	// The fixture is created with generation 0 and the condition observes 0, so
	// the state must mirror the condition rather than report "not observed yet".

	state, err := service.Ensure(context.Background(), adapter.NewAddonRepository(repo))
	if err != nil {
		t.Fatalf("Ensure returned %v", err)
	}
	if state.Ready {
		t.Fatal("state must mirror Ready=False from the internal object")
	}
	if state.Reason != "FetchFailed" || state.Message != "failed to fetch index" {
		t.Fatalf("state must translate reason and message, got %q / %q", state.Reason, state.Message)
	}
}

func TestHelmRepositoriesEnsureReportsStalled(t *testing.T) {
	repo := testRepository()
	internal := &sourcev1.HelmRepository{
		ObjectMeta: metav1.ObjectMeta{
			Name:       utils.GetInternalHelmRepositoryName(repo.Name),
			Namespace:  testNamespace,
			Generation: 1,
		},
		Status: sourcev1.HelmRepositoryStatus{
			Conditions: []metav1.Condition{
				{
					Type:               helmv1alpha1.ConditionTypeStalled,
					Status:             metav1.ConditionTrue,
					Reason:             "InvalidSecretRef",
					Message:            "secret not found",
					ObservedGeneration: 1,
					LastTransitionTime: metav1.Now(),
				},
			},
		},
	}

	service := newHelmRepositories(t, repo, internal)

	state, err := service.Ensure(context.Background(), adapter.NewAddonRepository(repo))
	if err != nil {
		t.Fatalf("Ensure returned %v", err)
	}
	if !state.Stalled {
		t.Fatal("state must report Stalled from the internal object")
	}
	if state.Reason != "InvalidSecretRef" {
		t.Fatalf("state reason is %q, want %q", state.Reason, "InvalidSecretRef")
	}
}

// TestHelmRepositoriesEnsureIgnoresAStallFromAnOlderGeneration covers a repository
// whose secret reference has just been fixed. source-controller drops Stalled at the
// head of the pass that acts on the new spec, so in the gap before that pass starts
// the condition of the rejected spec is still there at generation 2; reading it
// would report as abandoned a repository that is about to be fetched again. The
// fixture matches what applyHelmRepositorySpec writes, so
// CreateOrPatch is a no-op and the generation stays where it is put.
func TestHelmRepositoriesEnsureIgnoresAStallFromAnOlderGeneration(t *testing.T) {
	repo := testRepository()
	internal := &sourcev1.HelmRepository{
		ObjectMeta: metav1.ObjectMeta{
			Name:       utils.GetInternalHelmRepositoryName(repo.Name),
			Namespace:  testNamespace,
			Generation: 2,
			Labels: map[string]string{
				helmv1alpha1.LabelManagedBy:                            helmv1alpha1.LabelManagedByValue,
				helmv1alpha1.HelmClusterAddonRepositoryLabelSourceName: repo.Name,
			},
		},
		Spec: sourcev1.HelmRepositorySpec{
			URL:      repo.Spec.URL,
			Interval: metav1.Duration{Duration: InternalRepositoryInterval},
		},
		Status: sourcev1.HelmRepositoryStatus{
			Conditions: []metav1.Condition{
				{
					Type:               helmv1alpha1.ConditionTypeStalled,
					Status:             metav1.ConditionTrue,
					Reason:             "InvalidSecretRef",
					Message:            "secret not found",
					ObservedGeneration: 1,
					LastTransitionTime: metav1.Now(),
				},
			},
		},
	}

	service := newHelmRepositories(t, repo, internal)

	state, err := service.Ensure(context.Background(), adapter.NewAddonRepository(repo))
	if err != nil {
		t.Fatalf("Ensure returned %v", err)
	}
	if state.Stalled {
		t.Fatal("a Stalled condition of an older generation must not be read as the current verdict")
	}
	if state.Ready || state.Reason != helmv1alpha1.ReasonReconciling {
		t.Fatalf("state = %+v, want it waiting for the internal repository", state)
	}
}

// TestHelmRepositoriesEnsureStalledPrecedesReady pins the precedence rule:
// Stalled=True must win even when a healthy, observed Ready=True condition sits
// right next to it. The fixture's spec and labels already match what
// applyHelmRepositorySpec writes (same reason as the mirroring test above), so
// CreateOrPatch is a no-op and the internal object's generation stays at 1 -
// which is what lets the Ready condition below count as observed.
func TestHelmRepositoriesEnsureStalledPrecedesReady(t *testing.T) {
	repo := testRepository()
	internal := &sourcev1.HelmRepository{
		ObjectMeta: metav1.ObjectMeta{
			Name:       utils.GetInternalHelmRepositoryName(repo.Name),
			Namespace:  testNamespace,
			Generation: 1,
			Labels: map[string]string{
				helmv1alpha1.LabelManagedBy:                            helmv1alpha1.LabelManagedByValue,
				helmv1alpha1.HelmClusterAddonRepositoryLabelSourceName: repo.Name,
			},
		},
		Spec: sourcev1.HelmRepositorySpec{
			URL:      repo.Spec.URL,
			Interval: metav1.Duration{Duration: InternalRepositoryInterval},
		},
		Status: sourcev1.HelmRepositoryStatus{
			Conditions: []metav1.Condition{
				{
					Type:               helmv1alpha1.ConditionTypeReady,
					Status:             metav1.ConditionTrue,
					Reason:             "Succeeded",
					Message:            "index fetched",
					ObservedGeneration: 1,
					LastTransitionTime: metav1.Now(),
				},
				{
					Type:               helmv1alpha1.ConditionTypeStalled,
					Status:             metav1.ConditionTrue,
					Reason:             "InvalidSecretRef",
					Message:            "secret not found",
					ObservedGeneration: 1,
					LastTransitionTime: metav1.Now(),
				},
			},
		},
	}

	service := newHelmRepositories(t, repo, internal)

	state, err := service.Ensure(context.Background(), adapter.NewAddonRepository(repo))
	if err != nil {
		t.Fatalf("Ensure returned %v", err)
	}
	if !state.Stalled {
		t.Fatal("Stalled=True must take precedence even when an observed Ready=True is also present")
	}
	if state.Ready {
		t.Fatal("state must not report Ready when Stalled=True takes precedence")
	}
	if state.Reason != "InvalidSecretRef" {
		t.Fatalf("state reason is %q, want the Stalled reason %q, not the Ready reason", state.Reason, "InvalidSecretRef")
	}
}

// TestHelmRepositoriesEnsureReturnsAPIError verifies the split this task
// exists to create: an API failure while reconciling the internal object is the
// caller's problem and comes back as a non-nil error (still with Present: true,
// since the internal object does exist as far as the caller is concerned), not
// swallowed into the state. The failure is injected on Create because the fixture
// has no pre-existing internal HelmRepository, so CreateOrPatch's Get finds
// nothing and falls through to Create.
func TestHelmRepositoriesEnsureReturnsAPIError(t *testing.T) {
	repo := testRepository()
	scheme := sourceScheme(t)

	sentinel := errors.New("synthetic create failure")
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(repo).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, _ client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				return sentinel
			},
		}).
		Build()

	service := NewHelmRepositories(c, testNamespace)

	state, err := service.Ensure(context.Background(), adapter.NewAddonRepository(repo))
	if err == nil {
		t.Fatal("Ensure must return an error when the API call fails")
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("returned error must wrap the underlying API failure, got %v", err)
	}
	if !state.Present {
		t.Fatal("state must still report Present: true even when reconciling failed")
	}
}

// TestHelmRepositoriesEnsureKeepsForeignLabels pins that the internal
// HelmRepository follows the same label policy as every other internal object: a
// label somebody else put on it survives, while an owned label that is no longer
// desired goes away.
func TestHelmRepositoriesEnsureKeepsForeignLabels(t *testing.T) {
	repo := testRepository()
	internal := &sourcev1.HelmRepository{
		ObjectMeta: metav1.ObjectMeta{
			Name:      utils.GetInternalHelmRepositoryName(repo.Name),
			Namespace: testNamespace,
			Labels: map[string]string{
				"cost-center":                     "team-a",
				helmv1alpha1.LabelSourceNamespace: "gone",
			},
		},
	}

	scheme := sourceScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(repo, internal).Build()
	service := NewHelmRepositories(c, testNamespace)

	if _, err := service.Ensure(context.Background(), adapter.NewAddonRepository(repo)); err != nil {
		t.Fatalf("Ensure returned %v", err)
	}

	got := &sourcev1.HelmRepository{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(internal), got); err != nil {
		t.Fatalf("getting helm repository: %v", err)
	}

	if got.Labels["cost-center"] != "team-a" {
		t.Fatalf("labels = %v, want the foreign label preserved", got.Labels)
	}
	if _, found := got.Labels[helmv1alpha1.LabelSourceNamespace]; found {
		t.Fatalf("labels = %v, want the owned label a cluster-scoped source does not set removed", got.Labels)
	}
	for key, want := range adapter.NewAddonRepository(repo).SourceLabels() {
		if got.Labels[key] != want {
			t.Fatalf("label %q = %q, want %q", key, got.Labels[key], want)
		}
	}
}

// TestHelmRepositoriesEnsureClearsPassCredentialsWithTheSecretItBelongsTo pins the
// field to the reference that justifies it. spec.passCredentials tells
// source-controller to keep sending the repository's credentials when a request is
// redirected to another host; it is only ever meaningful next to a secretRef. A
// repository whose auth is taken away has its secretRef cleared on the same pass, and
// leaving the permission behind would hand it to whatever credentials the repository
// is given next.
func TestHelmRepositoriesEnsureClearsPassCredentialsWithTheSecretItBelongsTo(t *testing.T) {
	repo := testRepository()
	internal := &sourcev1.HelmRepository{
		ObjectMeta: metav1.ObjectMeta{
			Name:      utils.GetInternalHelmRepositoryName(repo.Name),
			Namespace: testNamespace,
		},
		Spec: sourcev1.HelmRepositorySpec{
			SecretRef:       &meta.LocalObjectReference{Name: "left-over"},
			PassCredentials: true,
		},
	}

	scheme := sourceScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(repo, internal).Build()
	service := NewHelmRepositories(c, testNamespace)

	if _, err := service.Ensure(context.Background(), adapter.NewAddonRepository(repo)); err != nil {
		t.Fatalf("Ensure returned %v", err)
	}

	got := &sourcev1.HelmRepository{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(internal), got); err != nil {
		t.Fatalf("getting helm repository: %v", err)
	}

	if got.Spec.SecretRef != nil {
		t.Fatalf("secretRef = %v, want it cleared with the auth it came from", got.Spec.SecretRef)
	}
	if got.Spec.PassCredentials {
		t.Fatal("passCredentials outlived the secretRef it belongs to")
	}
}

// TestHelmRepositoriesEnsureSetsPassCredentialsWithTheAuthSecret is the other side:
// the field is still set for a repository that does carry credentials.
func TestHelmRepositoriesEnsureSetsPassCredentialsWithTheAuthSecret(t *testing.T) {
	repo := testRepository()
	repo.Spec.Auth = &helmv1alpha1.RepositoryAuth{Username: "user", Password: "secret"}

	scheme := sourceScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(repo).Build()
	service := NewHelmRepositories(c, testNamespace)

	if _, err := service.Ensure(context.Background(), adapter.NewAddonRepository(repo)); err != nil {
		t.Fatalf("Ensure returned %v", err)
	}

	got := &sourcev1.HelmRepository{}
	key := client.ObjectKey{Namespace: testNamespace, Name: utils.GetInternalHelmRepositoryName(repo.Name)}
	if err := c.Get(context.Background(), key, got); err != nil {
		t.Fatalf("getting helm repository: %v", err)
	}

	if got.Spec.SecretRef == nil {
		t.Fatal("secretRef must name the mirrored auth secret")
	}
	if !got.Spec.PassCredentials {
		t.Fatal("passCredentials must be set for a repository that carries credentials")
	}
}
