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

	sourcev1 "github.com/werf/nelm-source-controller/api/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
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

func newHelmRepoService(t *testing.T, objects ...client.Object) *HelmRepoService {
	t.Helper()

	scheme := sourceScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()

	return NewHelmRepoService(c, scheme, testNamespace)
}

func testRepository() *helmv1alpha1.HelmClusterAddonRepository {
	return &helmv1alpha1.HelmClusterAddonRepository{
		ObjectMeta: metav1.ObjectMeta{Name: "example", Generation: 1},
		Spec:       helmv1alpha1.HelmClusterAddonRepositorySpec{URL: "https://example.invalid/charts"},
	}
}

func TestEnsureInternalHelmRepositoryReportsNotObservedAsNotReady(t *testing.T) {
	repo := testRepository()
	service := newHelmRepoService(t, repo)

	state, err := service.EnsureInternalHelmRepository(context.Background(), repo)
	if err != nil {
		t.Fatalf("EnsureInternalHelmRepository returned %v", err)
	}
	if !state.Present {
		t.Fatal("helm repositories must report an internal object")
	}
	if state.Ready {
		t.Fatal("a freshly created internal object must not be reported ready")
	}
}

func TestEnsureInternalHelmRepositoryMirrorsConditions(t *testing.T) {
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

	service := newHelmRepoService(t, repo, internal)

	// The fixture is created with generation 0 and the condition observes 0, so
	// the state must mirror the condition rather than report "not observed yet".

	state, err := service.EnsureInternalHelmRepository(context.Background(), repo)
	if err != nil {
		t.Fatalf("EnsureInternalHelmRepository returned %v", err)
	}
	if state.Ready {
		t.Fatal("state must mirror Ready=False from the internal object")
	}
	if state.Reason != "FetchFailed" || state.Message != "failed to fetch index" {
		t.Fatalf("state must translate reason and message, got %q / %q", state.Reason, state.Message)
	}
}

func TestEnsureInternalHelmRepositoryReportsStalled(t *testing.T) {
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

	service := newHelmRepoService(t, repo, internal)

	state, err := service.EnsureInternalHelmRepository(context.Background(), repo)
	if err != nil {
		t.Fatalf("EnsureInternalHelmRepository returned %v", err)
	}
	if !state.Stalled {
		t.Fatal("state must report Stalled from the internal object")
	}
	if state.Reason != "InvalidSecretRef" {
		t.Fatalf("state reason is %q, want %q", state.Reason, "InvalidSecretRef")
	}
}

// TestEnsureInternalHelmRepositoryStalledPrecedesReady pins the precedence rule:
// Stalled=True must win even when a healthy, observed Ready=True condition sits
// right next to it. The fixture's spec and labels already match what
// applyHelmRepositorySpec writes (same reason as the mirroring test above), so
// CreateOrPatch is a no-op and the internal object's generation stays at 1 -
// which is what lets the Ready condition below count as observed.
func TestEnsureInternalHelmRepositoryStalledPrecedesReady(t *testing.T) {
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

	service := newHelmRepoService(t, repo, internal)

	state, err := service.EnsureInternalHelmRepository(context.Background(), repo)
	if err != nil {
		t.Fatalf("EnsureInternalHelmRepository returned %v", err)
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

// TestEnsureInternalHelmRepositoryReturnsAPIError verifies the split this task
// exists to create: an API failure while reconciling the internal object is the
// caller's problem and comes back as a non-nil error (still with Present: true,
// since the internal object does exist as far as the caller is concerned), not
// swallowed into the state. The failure is injected on Create because the fixture
// has no pre-existing internal HelmRepository, so CreateOrPatch's Get finds
// nothing and falls through to Create.
func TestEnsureInternalHelmRepositoryReturnsAPIError(t *testing.T) {
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

	service := NewHelmRepoService(c, scheme, testNamespace)

	state, err := service.EnsureInternalHelmRepository(context.Background(), repo)
	if err == nil {
		t.Fatal("EnsureInternalHelmRepository must return an error when the API call fails")
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("returned error must wrap the underlying API failure, got %v", err)
	}
	if !state.Present {
		t.Fatal("state must still report Present: true even when reconciling failed")
	}
}
