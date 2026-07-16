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

	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
)

// DeletingResource is an internal resource that is being deleted; its deletion
// timestamp and conditions are used to derive the owner's status while the
// deletion is pending.
type DeletingResource interface {
	GetDeletionTimestamp() *metav1.Time
	GetConditions() []metav1.Condition
}

// readyResult sets the Ready condition directly from a Status.
type readyResult struct {
	status Status
}

var _ Provider = readyResult{}

func (r readyResult) GetStatus() Status { return r.status }

func (r readyResult) GetConditionType() string { return helmv1alpha1.ConditionTypeReady }

// uninstallFailedResult carries the state of the UninstallFailed condition, an
// abnormal-true condition that is True when a Helm release fails to uninstall
// while its owner is being deleted. The same result is reflected onto the Ready
// condition with inverted polarity (see reflectUninstallFailedToReady).
type uninstallFailedResult struct {
	status Status
}

var _ Provider = uninstallFailedResult{}

func (r uninstallFailedResult) GetStatus() Status { return r.status }

func (r uninstallFailedResult) GetConditionType() string {
	return helmv1alpha1.ConditionTypeUninstallFailed
}

// reflectUninstallFailedToReady inverts the condition status when the
// UninstallFailed result is reflected onto the Ready condition: an
// UninstallFailed=True (an error occurred) must read as Ready=False, while the
// reason and message are preserved. Unknown stays Unknown, so an in-progress
// uninstall reflects as Ready=Unknown/Reconciling.
func reflectUninstallFailedToReady(conditionType string, s Status) Status {
	if conditionType != helmv1alpha1.ConditionTypeReady {
		return s
	}

	switch s.Status {
	case metav1.ConditionTrue:
		s.Status = metav1.ConditionFalse
	case metav1.ConditionFalse:
		s.Status = metav1.ConditionTrue
	}

	return s
}

// MarkUninstallPending records that a Helm release is still being uninstalled
// while its owner is deleted. A failing release (its Ready condition is False)
// makes the UninstallFailed condition True and Ready False, both with reason
// UninstallFailed and the release's message; otherwise the owner stays
// Reconciling until the release disappears. resourceName is a user-facing,
// abstract name so the internal resource type is not leaked.
func (s *Manager) MarkUninstallPending(ctx context.Context, obj ObjectWithConditions, resourceName string, resource DeletingResource) error {
	var st Status
	if failing, message := deletionFailure(resourceName, resource); failing {
		st = Status{
			Observed:           true,
			Status:             metav1.ConditionTrue,
			Reason:             helmv1alpha1.ReasonUninstallFailed,
			Message:            message,
			ObservedGeneration: obj.GetGeneration(),
		}
	} else {
		st = reconcilingDeletionStatus(obj, message)
	}

	result := uninstallFailedResult{status: st}

	return s.Update(
		ctx,
		obj,
		NoopStatusMutator,
		reflectUninstallFailedToReady,
		result,
		AsCondition(result, helmv1alpha1.ConditionTypeReady),
	)
}

// MarkDeletionPending records that an internal resource is still being deleted,
// manipulating only the Ready condition — there is nothing to uninstall. A
// failing resource (its Ready condition is False) yields Ready=False with reason
// Failed and its message; otherwise Ready stays Unknown/Reconciling until the
// resource disappears. resourceName is a user-facing, abstract name.
func (s *Manager) MarkDeletionPending(ctx context.Context, obj ObjectWithConditions, resourceName string, resource DeletingResource) error {
	var st Status
	if failing, message := deletionFailure(resourceName, resource); failing {
		st = Status{
			Observed:           true,
			Status:             metav1.ConditionFalse,
			Reason:             helmv1alpha1.ReasonFailed,
			Message:            message,
			ObservedGeneration: obj.GetGeneration(),
		}
	} else {
		st = reconcilingDeletionStatus(obj, message)
	}

	return s.Update(ctx, obj, NoopStatusMutator, NoopStatusMapper, readyResult{status: st})
}

// MarkDeletionFailed sets Ready=False with reason Failed when an internal
// resource could not be deleted because of a hard error that is not reported
// through the resource's own conditions (e.g. an API error while deleting a
// dependency). The message uses the same abstract wording as MarkDeletionPending.
func (s *Manager) MarkDeletionFailed(ctx context.Context, obj ObjectWithConditions, resourceName string, err error) error {
	message := fmt.Sprintf("Failed to delete %s", resourceName)
	if err != nil {
		message = fmt.Sprintf("%s: %s", message, err.Error())
	}

	return s.Update(ctx, obj, NoopStatusMutator, NoopStatusMapper, readyResult{status: Status{
		Observed:           true,
		Status:             metav1.ConditionFalse,
		Reason:             helmv1alpha1.ReasonFailed,
		Message:            message,
		ObservedGeneration: obj.GetGeneration(),
		Err:                err,
	}})
}

func reconcilingDeletionStatus(obj ObjectWithConditions, message string) Status {
	return Status{
		Status:             metav1.ConditionUnknown,
		Reason:             helmv1alpha1.ReasonReconciling,
		Message:            message,
		ObservedGeneration: obj.GetGeneration(),
	}
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
