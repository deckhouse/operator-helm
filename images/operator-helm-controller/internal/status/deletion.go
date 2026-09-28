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

// DeletingResource is an internal resource that is being deleted; its deletion timestamp and
// conditions derive the owner's status while the deletion is pending.
type DeletingResource interface {
	client.Object

	GetConditions() []metav1.Condition
}

// MarkUninstallPending records that a Helm release is still being uninstalled while its owner
// is deleted. A failing release sets UninstallFailed True and Ready False.
func (s *Manager) MarkUninstallPending(ctx context.Context, obj ObjectWithConditions, resourceName string, resource DeletingResource) error {
	failing, message := deletionFailure(resourceName, resource)

	uninstallFailed := metav1.Condition{
		Type:               helmv1alpha1.ConditionTypeUninstallFailed,
		Status:             metav1.ConditionUnknown,
		Reason:             helmv1alpha1.ReasonReconciling,
		Message:            message,
		ObservedGeneration: obj.GetGeneration(),
	}

	// Ready carries the same verdict with the polarity a reader expects.
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

// MarkDeletionPending records that an internal resource is still being deleted, setting only
// Ready: False with reason Failed for a failing resource, Unknown otherwise.
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

// MarkDeletionFailed sets Ready=False with reason Failed when an internal resource could not
// be deleted because of an error its own conditions do not report.
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

// patchConditions writes the conditions of a deletion pass and advances observedGeneration.
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

// deletionFailure reports whether the deleted resource is failing, with a user-facing message.
func deletionFailure(resourceName string, resource DeletingResource) (bool, string) {
	waiting := fmt.Sprintf("Waiting for %s to be deleted", resourceName)

	// Before the deletion starts a Ready=False describes a state prior to it, not the teardown.
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
