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
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
)

// InternalObjectState describes the observed state of one internal FluxCD object a release
// owns. Observed is false until the object reports a verdict for the spec it was given.
type InternalObjectState struct {
	Observed bool
	// Stalled reports that the internal object gave up on the spec it was given: it
	// exhausted its remediation attempts, or reached a fault it will not retry.
	Stalled bool
	Status  metav1.ConditionStatus
	Reason  string
	Message string
}

// Ready reports the state every consumer means by it: the object has observed the
// spec it was given and calls itself ready for it.
func (s InternalObjectState) Ready() bool {
	return s.Observed && s.Status == metav1.ConditionTrue
}

// ErrorConditionRule maps a condition an internal FluxCD object raises about itself to
// the reason the release reports because of it. The rules of one kind are checked in order.
type ErrorConditionRule struct {
	Type          string
	TriggerStatus metav1.ConditionStatus
	Reason        string
}

func reduceInternalConditions(
	conditions []metav1.Condition,
	generation int64,
	errorRules []ErrorConditionRule,
) InternalObjectState {
	stalled := currentCondition(conditions, helmv1alpha1.ConditionTypeStalled, generation)
	gaveUp := stalled != nil && stalled.Status == metav1.ConditionTrue

	reconciling := InternalObjectState{
		Status: metav1.ConditionUnknown,
		Reason: helmv1alpha1.ReasonReconciling,
	}

	// Order matters: having given up outranks a Reconciling condition the attempt that gave up left behind.
	if !gaveUp {
		cond := currentCondition(conditions, helmv1alpha1.ConditionTypeReconciling, generation)
		if cond != nil && cond.Status == metav1.ConditionTrue && cond.Reason != helmv1alpha1.ReasonProgressingWithRetry {
			return reconciling
		}
	}

	for _, rule := range errorRules {
		if cond := currentCondition(conditions, rule.Type, generation); cond != nil && cond.Status == rule.TriggerStatus {
			return InternalObjectState{
				Observed: true,
				Stalled:  gaveUp,
				Status:   metav1.ConditionFalse,
				Reason:   rule.Reason,
				Message:  cond.Message,
			}
		}
	}

	ready, observed := conditionObserved(conditions, helmv1alpha1.ConditionTypeReady, generation)
	if !observed {
		if gaveUp {
			// No rule matched and no verdict was reached, so the object's own account is all there is.
			return InternalObjectState{
				Observed: true,
				Stalled:  true,
				Status:   metav1.ConditionFalse,
				Reason:   stalled.Reason,
				Message:  stalled.Message,
			}
		}

		return reconciling
	}

	return InternalObjectState{
		Observed: true,
		Stalled:  gaveUp,
		Status:   ready.Status,
		Reason:   ready.Reason,
		Message:  ready.Message,
	}
}

func conditionObserved(conditions []metav1.Condition, conditionType string, generation int64) (*metav1.Condition, bool) {
	// Stricter than currentCondition: Ready is the one condition read as proof the spec was observed.
	cond := apimeta.FindStatusCondition(conditions, conditionType)
	if cond == nil || cond.ObservedGeneration != generation {
		return cond, false
	}

	return cond, true
}

func currentCondition(conditions []metav1.Condition, conditionType string, generation int64) *metav1.Condition {
	// A condition with no observedGeneration counts as current: both controllers stamp what they write.
	cond := apimeta.FindStatusCondition(conditions, conditionType)
	if cond == nil || (cond.ObservedGeneration != 0 && cond.ObservedGeneration != generation) {
		return nil
	}

	return cond
}
