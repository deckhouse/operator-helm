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
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
)

// InternalObjectState describes the observed state of one internal FluxCD object a
// release owns — its HelmChart, its OCIRepository, its HelmRelease. It is the
// release-side counterpart of InternalRepositoryState: the service reads the
// object's conditions and reduces them to this, so the knowledge of which FluxCD
// condition means what stays where the object is written.
//
// An object the pass never reconciled has no state at all: the outcome carrying it
// is absent instead. Observed is false while the object has not yet reported a
// verdict for the spec it was given, which is why Status is kept whole rather than
// reduced to a Ready boolean — an object may report Unknown for a generation it has
// observed, and that is neither readiness nor failure.
type InternalObjectState struct {
	Observed bool
	Status   metav1.ConditionStatus
	Reason   string
	Message  string
}

// Ready reports the state every consumer means by it: the object has observed the
// spec it was given and calls itself ready for it.
func (s InternalObjectState) Ready() bool {
	return s.Observed && s.Status == metav1.ConditionTrue
}

// ErrorConditionRule maps a condition an internal FluxCD object raises about itself
// to the reason the release reports because of it. The rules of one kind are checked
// in order, so a more specific cause listed first wins over a general one.
type ErrorConditionRule struct {
	Type          string
	TriggerStatus metav1.ConditionStatus
	Reason        string
}

// reduceInternalConditions collapses the conditions of an internal object into the
// state the release evaluation works from. A running reconciliation outranks
// everything: whatever the object still says about the previous spec is stale until
// it settles. ProgressingWithRetry is excluded because it is not a reconciliation in
// flight but a failure that has scheduled one, and the failure is what the release
// has to report. After that the error rules speak, and only then the object's own
// Ready — which counts only once observed for the generation that was written.
func reduceInternalConditions(
	conditions []metav1.Condition,
	generation int64,
	errorRules []ErrorConditionRule,
) InternalObjectState {
	reconciling := InternalObjectState{
		Status: metav1.ConditionUnknown,
		Reason: helmv1alpha1.ReasonReconciling,
	}

	cond := apimeta.FindStatusCondition(conditions, helmv1alpha1.ConditionTypeReconciling)
	if cond != nil && cond.Status == metav1.ConditionTrue && cond.Reason != helmv1alpha1.ReasonProgressingWithRetry {
		return reconciling
	}

	for _, rule := range errorRules {
		if cond := apimeta.FindStatusCondition(conditions, rule.Type); cond != nil && cond.Status == rule.TriggerStatus {
			return InternalObjectState{
				Observed: true,
				Status:   metav1.ConditionFalse,
				Reason:   rule.Reason,
				Message:  cond.Message,
			}
		}
	}

	ready, observed := conditionObserved(conditions, helmv1alpha1.ConditionTypeReady, generation)
	if !observed {
		return reconciling
	}

	return InternalObjectState{
		Observed: true,
		Status:   ready.Status,
		Reason:   ready.Reason,
		Message:  ready.Message,
	}
}

// conditionObserved returns the named condition only when it was set for the
// generation given, so a verdict about a spec that has since changed is not read as
// one about the current spec.
func conditionObserved(conditions []metav1.Condition, conditionType string, generation int64) (*metav1.Condition, bool) {
	cond := apimeta.FindStatusCondition(conditions, conditionType)
	if cond == nil || cond.ObservedGeneration != generation {
		return cond, false
	}

	return cond, true
}
