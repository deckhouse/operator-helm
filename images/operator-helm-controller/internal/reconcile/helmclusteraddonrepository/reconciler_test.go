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

package helmclusteraddonrepository

import (
	"context"
	"testing"
	"time"

	"github.com/Masterminds/semver/v3"
	sourcev1 "github.com/werf/nelm-source-controller/api/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
	repoclient "github.com/deckhouse/operator-helm/internal/client/repository"
	"github.com/deckhouse/operator-helm/internal/manager/status"
	"github.com/deckhouse/operator-helm/internal/services"
	"github.com/deckhouse/operator-helm/internal/utils"
)

type stubRepoClient struct {
	charts []repoclient.Chart
	err    error
}

// The receiver is a pointer so a test can change what the repository returns
// between reconcile passes.
func (s *stubRepoClient) FetchCharts(_ context.Context, _ string, _ *repoclient.RepoConfig) ([]repoclient.Chart, error) {
	return s.charts, s.err
}

func newReconciler(t *testing.T, stub *stubRepoClient, objects ...client.Object) (*Reconciler, client.Client) {
	t.Helper()

	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		clientgoscheme.AddToScheme,
		helmv1alpha1.AddToScheme,
		sourcev1.AddToScheme,
	} {
		if err := add(scheme); err != nil {
			t.Fatalf("registering scheme: %v", err)
		}
	}

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objects...).
		WithStatusSubresource(
			&helmv1alpha1.HelmClusterAddonRepository{},
			&helmv1alpha1.HelmClusterAddonChart{},
		).
		Build()

	factory := func(_ utils.InternalRepositoryType) (repoclient.ClientInterface, error) {
		return stub, nil
	}

	r := New(
		c,
		services.NewHelmRepoService(c, scheme, helmv1alpha1.TargetNamespace),
		services.NewOCIRepoService(c, scheme, helmv1alpha1.TargetNamespace),
		services.NewRepoSyncService(c, scheme, factory),
		status.NewManager(c),
	)

	return r, c
}

func ociRepository() *helmv1alpha1.HelmClusterAddonRepository {
	return &helmv1alpha1.HelmClusterAddonRepository{
		ObjectMeta: metav1.ObjectMeta{Name: "example", Generation: 1},
		Spec:       helmv1alpha1.HelmClusterAddonRepositorySpec{URL: "oci://ghcr.io/example/podinfo"},
	}
}

func reconcileUntilStable(t *testing.T, r *Reconciler, name string) reconcile.Result {
	t.Helper()

	var result reconcile.Result
	// The first pass only adds the finalizer path; two passes are enough to reach
	// a stable state for a repository whose source responds.
	for range 2 {
		var err error
		result, err = r.Reconcile(context.Background(), reconcile.Request{
			NamespacedName: types.NamespacedName{Name: name},
		})
		if err != nil {
			t.Fatalf("Reconcile returned %v", err)
		}
	}

	return result
}

func TestReconcileOCIRepositoryBecomesReady(t *testing.T) {
	repo := ociRepository()
	stub := &stubRepoClient{charts: []repoclient.Chart{{
		Name:     "podinfo",
		Versions: []repoclient.ChartVersion{{Version: semver.MustParse("6.7.1")}},
	}}}

	r, c := newReconciler(t, stub, repo)
	result := reconcileUntilStable(t, r, repo.Name)

	if result.RequeueAfter <= 0 || result.RequeueAfter > time.Hour {
		t.Fatalf("expected a scheduled requeue, got %s", result.RequeueAfter)
	}

	updated := &helmv1alpha1.HelmClusterAddonRepository{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(repo), updated); err != nil {
		t.Fatalf("getting repository: %v", err)
	}

	if !apimeta.IsStatusConditionTrue(updated.Status.Conditions, helmv1alpha1.ConditionTypeReady) {
		t.Fatalf("Ready must be True, conditions: %v", updated.Status.Conditions)
	}
	if !apimeta.IsStatusConditionTrue(updated.Status.Conditions, helmv1alpha1.ConditionTypeSynced) {
		t.Fatal("Synced must be True")
	}
	if apimeta.FindStatusCondition(updated.Status.Conditions, helmv1alpha1.ConditionTypeReconciling) != nil {
		t.Fatal("Reconciling must be absent on a healthy repository")
	}
	if apimeta.FindStatusCondition(updated.Status.Conditions, helmv1alpha1.ConditionTypeStalled) != nil {
		t.Fatal("Stalled must be absent on a healthy repository")
	}
	if updated.Status.LastSuccessfulSyncTime == nil || updated.Status.NextSyncTime == nil {
		t.Fatal("sync timestamps must be recorded")
	}
}

func TestReconcileSkipsFetchBeforeSchedule(t *testing.T) {
	repo := ociRepository()
	stub := &stubRepoClient{charts: []repoclient.Chart{{
		Name:     "podinfo",
		Versions: []repoclient.ChartVersion{{Version: semver.MustParse("6.7.1")}},
	}}}

	r, c := newReconciler(t, stub, repo)
	reconcileUntilStable(t, r, repo.Name)

	before := &helmv1alpha1.HelmClusterAddonRepository{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(repo), before); err != nil {
		t.Fatalf("getting repository: %v", err)
	}

	// A watch-driven pass before nextSyncTime must not move the schedule.
	if _, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: repo.Name},
	}); err != nil {
		t.Fatalf("Reconcile returned %v", err)
	}

	after := &helmv1alpha1.HelmClusterAddonRepository{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(repo), after); err != nil {
		t.Fatalf("getting repository: %v", err)
	}

	if !after.Status.NextSyncTime.Time.Equal(before.Status.NextSyncTime.Time) {
		t.Fatalf("nextSyncTime moved without a due schedule: %s -> %s",
			before.Status.NextSyncTime.Time, after.Status.NextSyncTime.Time)
	}
}

func TestReconcileTerminalFetchFailureStalls(t *testing.T) {
	repo := ociRepository()
	stub := &stubRepoClient{err: &repoclient.TerminalError{
		Reason:  helmv1alpha1.ReasonAuthenticationFailed,
		Message: "repository rejected the credentials (HTTP 401)",
	}}

	r, c := newReconciler(t, stub, repo)
	reconcileUntilStable(t, r, repo.Name)

	updated := &helmv1alpha1.HelmClusterAddonRepository{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(repo), updated); err != nil {
		t.Fatalf("getting repository: %v", err)
	}

	stalled := apimeta.FindStatusCondition(updated.Status.Conditions, helmv1alpha1.ConditionTypeStalled)
	if stalled == nil || stalled.Reason != helmv1alpha1.ReasonAuthenticationFailed {
		t.Fatalf("expected Stalled=AuthenticationFailed, got %v", stalled)
	}
	if !apimeta.IsStatusConditionFalse(updated.Status.Conditions, helmv1alpha1.ConditionTypeReady) {
		t.Fatalf("Ready must be False while Stalled, conditions: %v", updated.Status.Conditions)
	}
	if apimeta.FindStatusCondition(updated.Status.Conditions, helmv1alpha1.ConditionTypeReconciling) != nil {
		t.Fatal("Reconciling and Stalled must be mutually exclusive")
	}
	// The fake client bumps generation on the finalizer update, so compare with
	// the live object rather than with the fixture.
	if updated.Status.ObservedGeneration != updated.Generation {
		t.Fatalf("observedGeneration is %d, want %d", updated.Status.ObservedGeneration, updated.Generation)
	}
}

// TestReconcileRemovesStalledOnRecovery pins that an abnormal-true condition is
// removed from the STORED object and not merely from the in-memory status.
// Removal rides on the JSON merge patch client.MergeFrom produces, which
// replaces the whole conditions array; were that ever to stop holding, a
// repository would keep reporting Failed to kstatus forever after one stall.
func TestReconcileRemovesStalledOnRecovery(t *testing.T) {
	repo := ociRepository()
	stub := &stubRepoClient{err: &repoclient.TerminalError{
		Reason:  helmv1alpha1.ReasonSourceNotFound,
		Message: "repository not found (HTTP 404)",
	}}

	r, c := newReconciler(t, stub, repo)
	reconcileUntilStable(t, r, repo.Name)

	stalled := &helmv1alpha1.HelmClusterAddonRepository{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(repo), stalled); err != nil {
		t.Fatalf("getting repository: %v", err)
	}
	if apimeta.FindStatusCondition(stalled.Status.Conditions, helmv1alpha1.ConditionTypeStalled) == nil {
		t.Fatalf("the fixture must reach Stalled first, conditions: %v", stalled.Status.Conditions)
	}

	// The source recovers. The force annotation makes the next pass attempt
	// regardless of the schedule the stall left behind.
	stub.err = nil
	stub.charts = []repoclient.Chart{{
		Name:     "podinfo",
		Versions: []repoclient.ChartVersion{{Version: semver.MustParse("6.7.1")}},
	}}

	stalled.Annotations = map[string]string{helmv1alpha1.AnnotationForceReconcile: ""}
	if err := c.Update(context.Background(), stalled); err != nil {
		t.Fatalf("annotating repository: %v", err)
	}

	if _, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: repo.Name},
	}); err != nil {
		t.Fatalf("Reconcile returned %v", err)
	}

	recovered := &helmv1alpha1.HelmClusterAddonRepository{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(repo), recovered); err != nil {
		t.Fatalf("getting repository: %v", err)
	}

	if cond := apimeta.FindStatusCondition(recovered.Status.Conditions, helmv1alpha1.ConditionTypeStalled); cond != nil {
		t.Fatalf("Stalled must be gone from the stored object, got %+v", cond)
	}
	if !apimeta.IsStatusConditionTrue(recovered.Status.Conditions, helmv1alpha1.ConditionTypeReady) {
		t.Fatalf("Ready must be True after recovery, conditions: %v", recovered.Status.Conditions)
	}
	if apimeta.FindStatusCondition(recovered.Status.Conditions, helmv1alpha1.ConditionTypeReconciling) != nil {
		t.Fatal("Reconciling must be absent on a recovered repository")
	}
	if _, found := recovered.Annotations[helmv1alpha1.AnnotationForceReconcile]; found {
		t.Fatal("the force annotation must be consumed by the pass it triggered")
	}
}

// TestReconcileDeleteCleansUpWhenURLNoLongerParses covers a repository whose url
// satisfies the CRD's validation regex but is rejected by url.Parse, so the
// repository type cannot be determined. Its internal objects were created while
// the url still parsed, so the deletion path must still remove them.
func TestReconcileDeleteCleansUpWhenURLNoLongerParses(t *testing.T) {
	now := metav1.Now()
	repo := &helmv1alpha1.HelmClusterAddonRepository{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "example",
			Generation:        1,
			Finalizers:        []string{helmv1alpha1.FinalizerName},
			DeletionTimestamp: &now,
		},
		// Passes the CRD rule ^(https?|oci)://.+$ and fails url.Parse.
		Spec: helmv1alpha1.HelmClusterAddonRepositorySpec{URL: "https://exa mple.invalid/charts"},
	}

	if _, err := utils.GetRepositoryType(repo.Spec.URL); err == nil {
		t.Fatal("the fixture url must be unparsable, otherwise the test proves nothing")
	}

	internalRepo := &sourcev1.HelmRepository{ObjectMeta: metav1.ObjectMeta{
		Name:      utils.GetInternalHelmRepositoryName(repo.Name),
		Namespace: helmv1alpha1.TargetNamespace,
	}}
	authSecret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name:      utils.GetInternalRepositoryAuthSecretName(repo.Name),
		Namespace: helmv1alpha1.TargetNamespace,
	}}
	tlsSecret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name:      utils.GetInternalRepositoryTLSSecretName(repo.Name),
		Namespace: helmv1alpha1.TargetNamespace,
	}}

	r, c := newReconciler(t, &stubRepoClient{}, repo, internalRepo, authSecret, tlsSecret)

	// The first pass deletes the internal objects and waits for the internal
	// repository to disappear; the second removes the finalizer.
	reconcileUntilStable(t, r, repo.Name)

	for _, obj := range []client.Object{internalRepo, authSecret, tlsSecret} {
		key := client.ObjectKeyFromObject(obj)
		if err := c.Get(context.Background(), key, obj.DeepCopyObject().(client.Object)); !apierrors.IsNotFound(err) {
			t.Fatalf("%s must be deleted, got %v", key, err)
		}
	}
}
