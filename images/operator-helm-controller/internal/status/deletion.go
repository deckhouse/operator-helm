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
	"fmt"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
)

// DeletingResource is an internal resource that is being deleted; its deletion
// timestamp and conditions are used to derive the owner's status while the
// deletion is pending. It is a client.Object so that a caller waiting on one can
// name it in the log; the resourceName passed alongside stays abstract because that
// one reaches the user through the owner's status.
type DeletingResource interface {
	client.Object

	GetConditions() []metav1.Condition
}

// MarkUninstallPending records that a Helm release is still being uninstalled
// while its owner is deleted. A failing release (its Ready condition is False)
// makes the UninstallFailed condition True and Ready False, both with reason
// UninstallFailed and the release's message; otherwise the owner stays
// Reconciling until the release disappears. resourceName is a user-facing,
// abstract name so the internal resource type is not leaked.
func (s *Manager) MarkUninstallPending(ctx context.Context, obj ObjectWithConditions, resourceName string, resource DeletingResource) error {
	failing, message := deletionFailure(resourceName, resource)

	uninstallFailed := metav1.Condition{
		Type:               helmv1alpha1.ConditionTypeUninstallFailed,
		Status:             metav1.ConditionUnknown,
		Reason:             helmv1alpha1.ReasonReconciling,
		Message:            message,
		ObservedGeneration: obj.GetGeneration(),
	}

	// Ready carries the same verdict with the polarity a reader expects: an
	// uninstall that failed is an owner that is not ready, while one still running
	// leaves both Unknown.
	ready := uninstallFailed
	ready.Type = helmv1alpha1.ConditionTypeReady

	if failing {
		uninstallFailed.Status = metav1.ConditionTrue
		uninstallFailed.Reason = helmv1alpha1.ReasonUninstallFailed
		ready.Status = metav1.ConditionFalse
		ready.Reason = helmv1alpha1.ReasonUninstallFailed
	}

	return s.patchConditions(ctx, obj, uninstallFailed, ready)
}

// MarkDeletionPending records that an internal resource is still being deleted,
// manipulating only the Ready condition — there is nothing to uninstall. A
// failing resource (its Ready condition is False) yields Ready=False with reason
// Failed and its message; otherwise Ready stays Unknown/Reconciling until the
// resource disappears. resourceName is a user-facing, abstract name.
func (s *Manager) MarkDeletionPending(ctx context.Context, obj ObjectWithConditions, resourceName string, resource DeletingResource) error {
	failing, message := deletionFailure(resourceName, resource)

	ready := metav1.Condition{
		Type:               helmv1alpha1.ConditionTypeReady,
		Status:             metav1.ConditionUnknown,
		Reason:             helmv1alpha1.ReasonReconciling,
		Message:            message,
		ObservedGeneration: obj.GetGeneration(),
	}

	if failing {
		ready.Status = metav1.ConditionFalse
		ready.Reason = helmv1alpha1.ReasonFailed
	}

	return s.patchConditions(ctx, obj, ready)
}

// MarkDeletionFailed sets Ready=False with reason Failed when an internal
// resource could not be deleted because of a hard error that is not reported
// through the resource's own conditions (e.g. an API error while deleting a
// dependency). The message uses the same abstract wording as MarkDeletionPending.
// err itself is not logged here: every caller hands it back to the work queue,
// which is where it is reported.
func (s *Manager) MarkDeletionFailed(ctx context.Context, obj ObjectWithConditions, resourceName string, err error) error {
	message := fmt.Sprintf("Failed to delete %s", resourceName)
	if err != nil {
		message = fmt.Sprintf("%s: %s", message, err.Error())
	}

	return s.patchConditions(ctx, obj, metav1.Condition{
		Type:               helmv1alpha1.ConditionTypeReady,
		Status:             metav1.ConditionFalse,
		Reason:             helmv1alpha1.ReasonFailed,
		Message:            message,
		ObservedGeneration: obj.GetGeneration(),
	})
}

// patchConditions writes the conditions of a deletion pass. observedGeneration is
// advanced with them: a spec edited while the object is being deleted is still a
// spec this controller has seen, and leaving it behind would report the teardown as
// work that has not started.
func (s *Manager) patchConditions(ctx context.Context, obj ObjectWithConditions, conditions ...metav1.Condition) error {
	return s.PatchStatus(ctx, obj, func() {
		for _, condition := range conditions {
			apimeta.SetStatusCondition(obj.GetConditions(), condition)
		}

		if generation := obj.GetGeneration(); generation > obj.GetObservedGeneration() {
			obj.SetObservedGeneration(generation)
		}
	})
}

// deletionFailure reports whether the deleted resource is failing, together with
// a user-facing message.
//
// The failure is only reported once the resource's own deletion has actually
// started (its deletionTimestamp is set). Before that, a Ready=False left over
// from a state prior to deletion (e.g. a failed install) does not describe the
// teardown and would be misleading — in that case the deletion has simply not
// been attempted yet, so it is treated as still in progress.
func deletionFailure(resourceName string, resource DeletingResource) (bool, string) {
	waiting := fmt.Sprintf("Waiting for %s to be deleted", resourceName)

	if resource.GetDeletionTimestamp().IsZero() {
		return false, waiting
	}

	ready := apimeta.FindStatusCondition(resource.GetConditions(), helmv1alpha1.ConditionTypeReady)
	if ready != nil && ready.Status == metav1.ConditionFalse {
		message := fmt.Sprintf("Failed to delete %s", resourceName)
		if ready.Message != "" {
			message = fmt.Sprintf("%s: %s", message, ready.Message)
		}

		return true, message
	}

	return false, waiting
}
