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

package status

import (
	"context"
	"errors"
	"strings"
	"testing"

	helmv2 "github.com/fluxcd/helm-controller/api/v2"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
)

func newDeletionManager(t *testing.T, owner *helmv1alpha1.HelmClusterAddon) (*Manager, client.Client) {
	t.Helper()

	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		clientgoscheme.AddToScheme,
		helmv1alpha1.AddToScheme,
		helmv2.AddToScheme,
	} {
		if err := add(scheme); err != nil {
			t.Fatalf("registering scheme: %v", err)
		}
	}

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(owner).
		WithStatusSubresource(&helmv1alpha1.HelmClusterAddon{}).
		Build()

	return NewManager(c), c
}

func deletionOwner() *helmv1alpha1.HelmClusterAddon {
	return &helmv1alpha1.HelmClusterAddon{
		ObjectMeta: metav1.ObjectMeta{Name: "addon", Generation: 3},
	}
}

// deletingRelease is an internal object partway through its own deletion, which is
// what makes its Ready condition describe the teardown rather than whatever came
// before it.
func deletingRelease(ready *metav1.Condition) *helmv2.HelmRelease {
	deleted := metav1.Now()
	release := &helmv2.HelmRelease{
		ObjectMeta: metav1.ObjectMeta{Name: "internal", DeletionTimestamp: &deleted, Finalizers: []string{"keep"}},
	}
	if ready != nil {
		release.Status.Conditions = []metav1.Condition{*ready}
	}

	return release
}

func storedConditions(t *testing.T, c client.Client, owner *helmv1alpha1.HelmClusterAddon) *helmv1alpha1.HelmClusterAddon {
	t.Helper()

	stored := &helmv1alpha1.HelmClusterAddon{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(owner), stored); err != nil {
		t.Fatalf("getting owner: %v", err)
	}

	return stored
}

func requireCondition(t *testing.T, owner *helmv1alpha1.HelmClusterAddon, conditionType string) metav1.Condition {
	t.Helper()

	cond := apimeta.FindStatusCondition(owner.Status.Conditions, conditionType)
	if cond == nil {
		t.Fatalf("condition %q was not written, got %+v", conditionType, owner.Status.Conditions)
	}

	return *cond
}

// TestMarkDeletionPendingWaits pins what an owner reports while an internal
// resource it must outlive is still going away: work in flight, not a failure.
func TestMarkDeletionPendingWaits(t *testing.T) {
	owner := deletionOwner()
	manager, c := newDeletionManager(t, owner)

	if err := manager.MarkDeletionPending(context.Background(), owner, "internal chart", deletingRelease(nil)); err != nil {
		t.Fatalf("MarkDeletionPending returned %v", err)
	}

	stored := storedConditions(t, c, owner)
	ready := requireCondition(t, stored, helmv1alpha1.ConditionTypeReady)

	if ready.Status != metav1.ConditionUnknown || ready.Reason != helmv1alpha1.ReasonReconciling {
		t.Fatalf("Ready = %s/%s, want Unknown/%s", ready.Status, ready.Reason, helmv1alpha1.ReasonReconciling)
	}
	if ready.Message != "Waiting for internal chart to be deleted" {
		t.Fatalf("message = %q, want the abstract resource name", ready.Message)
	}
	if ready.ObservedGeneration != owner.Generation {
		t.Fatalf("observedGeneration = %d, want %d", ready.ObservedGeneration, owner.Generation)
	}
	if stored.Status.ObservedGeneration != owner.Generation {
		t.Fatalf("status observedGeneration = %d, want %d", stored.Status.ObservedGeneration, owner.Generation)
	}
}

// TestMarkDeletionPendingReportsAFailingResource pins the other half: once the
// resource's own deletion has started, its Ready=False describes the teardown and
// the owner says so.
func TestMarkDeletionPendingReportsAFailingResource(t *testing.T) {
	owner := deletionOwner()
	manager, c := newDeletionManager(t, owner)

	resource := deletingRelease(&metav1.Condition{
		Type:    helmv1alpha1.ConditionTypeReady,
		Status:  metav1.ConditionFalse,
		Reason:  "Failed",
		Message: "finalizer stuck",
	})

	if err := manager.MarkDeletionPending(context.Background(), owner, "internal chart", resource); err != nil {
		t.Fatalf("MarkDeletionPending returned %v", err)
	}

	ready := requireCondition(t, storedConditions(t, c, owner), helmv1alpha1.ConditionTypeReady)
	if ready.Status != metav1.ConditionFalse || ready.Reason != helmv1alpha1.ReasonFailed {
		t.Fatalf("Ready = %s/%s, want False/%s", ready.Status, ready.Reason, helmv1alpha1.ReasonFailed)
	}
	if !strings.Contains(ready.Message, "finalizer stuck") {
		t.Fatalf("message = %q, want the resource's own cause carried over", ready.Message)
	}
}

// TestMarkDeletionPendingIgnoresAFailureFromBeforeTheDeletion pins the guard: a
// Ready=False left over from a failed install says nothing about the teardown, and
// reporting it as a deletion failure would be misleading.
func TestMarkDeletionPendingIgnoresAFailureFromBeforeTheDeletion(t *testing.T) {
	owner := deletionOwner()
	manager, c := newDeletionManager(t, owner)

	resource := &helmv2.HelmRelease{
		ObjectMeta: metav1.ObjectMeta{Name: "internal"},
		Status: helmv2.HelmReleaseStatus{Conditions: []metav1.Condition{{
			Type:    helmv1alpha1.ConditionTypeReady,
			Status:  metav1.ConditionFalse,
			Reason:  "InstallFailed",
			Message: "chart values rejected",
		}}},
	}

	if err := manager.MarkDeletionPending(context.Background(), owner, "internal release", resource); err != nil {
		t.Fatalf("MarkDeletionPending returned %v", err)
	}

	ready := requireCondition(t, storedConditions(t, c, owner), helmv1alpha1.ConditionTypeReady)
	if ready.Status != metav1.ConditionUnknown {
		t.Fatalf("Ready = %s, want Unknown: the deletion has not been attempted yet", ready.Status)
	}
}

// TestMarkUninstallPendingReportsAFailedUninstall pins the polarity of the two
// conditions: the abnormal-true one is raised, and Ready carries the same reason
// and message inverted.
func TestMarkUninstallPendingReportsAFailedUninstall(t *testing.T) {
	owner := deletionOwner()
	manager, c := newDeletionManager(t, owner)

	resource := deletingRelease(&metav1.Condition{
		Type:    helmv1alpha1.ConditionTypeReady,
		Status:  metav1.ConditionFalse,
		Reason:  "UninstallFailed",
		Message: "helm uninstall failed",
	})

	if err := manager.MarkUninstallPending(context.Background(), owner, "internal release", resource); err != nil {
		t.Fatalf("MarkUninstallPending returned %v", err)
	}

	stored := storedConditions(t, c, owner)
	failed := requireCondition(t, stored, helmv1alpha1.ConditionTypeUninstallFailed)
	ready := requireCondition(t, stored, helmv1alpha1.ConditionTypeReady)

	if failed.Status != metav1.ConditionTrue || failed.Reason != helmv1alpha1.ReasonUninstallFailed {
		t.Fatalf("UninstallFailed = %s/%s, want True/%s", failed.Status, failed.Reason, helmv1alpha1.ReasonUninstallFailed)
	}
	if ready.Status != metav1.ConditionFalse || ready.Reason != helmv1alpha1.ReasonUninstallFailed {
		t.Fatalf("Ready = %s/%s, want False/%s", ready.Status, ready.Reason, helmv1alpha1.ReasonUninstallFailed)
	}
	if ready.Message != failed.Message {
		t.Fatalf("Ready message %q and UninstallFailed message %q must be the same verdict", ready.Message, failed.Message)
	}
}

// TestMarkUninstallPendingLeavesBothUnknownWhileRunning pins that an uninstall
// still in flight is not a failure on either condition.
func TestMarkUninstallPendingLeavesBothUnknownWhileRunning(t *testing.T) {
	owner := deletionOwner()
	manager, c := newDeletionManager(t, owner)

	if err := manager.MarkUninstallPending(context.Background(), owner, "internal release", deletingRelease(nil)); err != nil {
		t.Fatalf("MarkUninstallPending returned %v", err)
	}

	stored := storedConditions(t, c, owner)
	for _, conditionType := range []string{helmv1alpha1.ConditionTypeUninstallFailed, helmv1alpha1.ConditionTypeReady} {
		cond := requireCondition(t, stored, conditionType)
		if cond.Status != metav1.ConditionUnknown || cond.Reason != helmv1alpha1.ReasonReconciling {
			t.Fatalf("%s = %s/%s, want Unknown/%s", conditionType, cond.Status, cond.Reason, helmv1alpha1.ReasonReconciling)
		}
	}
}

func TestMarkDeletionFailedNamesTheCause(t *testing.T) {
	owner := deletionOwner()
	manager, c := newDeletionManager(t, owner)

	err := manager.MarkDeletionFailed(context.Background(), owner, "auxiliary secrets", errors.New("forbidden"))
	if err != nil {
		t.Fatalf("MarkDeletionFailed returned %v", err)
	}

	ready := requireCondition(t, storedConditions(t, c, owner), helmv1alpha1.ConditionTypeReady)
	if ready.Status != metav1.ConditionFalse || ready.Reason != helmv1alpha1.ReasonFailed {
		t.Fatalf("Ready = %s/%s, want False/%s", ready.Status, ready.Reason, helmv1alpha1.ReasonFailed)
	}
	if !strings.Contains(ready.Message, "auxiliary secrets") || !strings.Contains(ready.Message, "forbidden") {
		t.Fatalf("message = %q, want both the resource and the cause", ready.Message)
	}
}

// TestPatchStatusSkipsAnUnchangedStatus pins the guard every status write relies
// on: repeating a verdict must not put a write on the API server.
func TestPatchStatusSkipsAnUnchangedStatus(t *testing.T) {
	owner := deletionOwner()
	manager, c := newDeletionManager(t, owner)

	if err := manager.MarkDeletionPending(context.Background(), owner, "internal chart", deletingRelease(nil)); err != nil {
		t.Fatalf("first MarkDeletionPending returned %v", err)
	}
	first := storedConditions(t, c, owner).ResourceVersion

	if err := manager.MarkDeletionPending(context.Background(), owner, "internal chart", deletingRelease(nil)); err != nil {
		t.Fatalf("second MarkDeletionPending returned %v", err)
	}
	if second := storedConditions(t, c, owner).ResourceVersion; second != first {
		t.Fatalf("resourceVersion moved from %s to %s: an unchanged status must not be patched", first, second)
	}
}
