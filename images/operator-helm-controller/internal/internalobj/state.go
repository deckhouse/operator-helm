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

// InternalObjectState describes the observed state of one internal FluxCD object a
// release owns — its HelmChart, its OCIRepository, its HelmRelease. It is the
// release-side counterpart of InternalRepositoryState: the gateway reads the
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
	// Stalled reports that the internal object gave up on the spec it was given:
	// it exhausted its remediation attempts, or reached a fault it will not retry.
	// Nothing on this side brings it back — the release has to be changed, which
	// gives the internal object a new spec to act on — so the release reports it as
	// Stalled rather than as work still in flight. It is kept apart from Status
	// because the two answer different questions: Status is the verdict, Stalled is
	// whether anything more is coming.
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

// ErrorConditionRule maps a condition an internal FluxCD object raises about itself
// to the reason the release reports because of it. The rules of one kind are checked
// in order, so a more specific cause listed first wins over a general one.
type ErrorConditionRule struct {
	Type          string
	TriggerStatus metav1.ConditionStatus
	Reason        string
}

// reduceInternalConditions collapses the conditions of an internal object into the
// state the release evaluation works from. Only conditions that speak about the
// generation the object currently has are read at all (see currentCondition):
// between the moment this side writes a new spec and the moment flux starts the pass
// that acts on it, the status still holds the verdict about the previous spec.
//
// Having given up outranks everything: an object that will not act on the spec it
// was given is not going to reach a verdict about it either, so its Reconciling
// condition — left behind by the attempt that gave up — must not be read as work in
// flight. Otherwise a running reconciliation comes first: whatever the object still
// says about the previous spec is stale until it settles, and ProgressingWithRetry
// is excluded there because it is not a reconciliation in flight but a failure that
// has scheduled one, and the failure is what the release has to report. After that
// the error rules speak, and only then the object's own Ready — which counts only
// once observed for the generation that was written.
//
// Whether the object gave up is carried alongside the verdict rather than replacing
// it: the error rules name the actual fault ("server-side apply failed …"), which is
// what someone reading the release needs, while the Stalled condition only says how
// many attempts were spent. The reason from Stalled is used solely where there is no
// other verdict to report.
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
			// No rule matched and no verdict was reached for this generation, so the
			// object's own account of why it stopped is all there is to report.
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

// conditionObserved returns the named condition only when it was set for the
// generation given, so a verdict about a spec that has since changed is not read as
// one about the current spec. It is deliberately stricter than currentCondition: a
// condition carrying no observedGeneration is no proof that the object has looked at
// the spec that was written, and Ready is the one condition read as such proof.
func conditionObserved(conditions []metav1.Condition, conditionType string, generation int64) (*metav1.Condition, bool) {
	cond := apimeta.FindStatusCondition(conditions, conditionType)
	if cond == nil || cond.ObservedGeneration != generation {
		return cond, false
	}

	return cond, true
}

// currentCondition returns the named condition unless it belongs to a generation
// other than the one the object has. Conditions are rewritten by the controller that
// owns the object, not by the write that changes its spec: helm-controller clears
// Stalled and raises Reconciling at the start of a pass (conditions.MarkReconciling),
// and source-controller reaches the same through reconcile.ProgressiveStatus. The
// stale window is therefore not the length of the next pass but the gap before it
// starts — from the patch that bumps the generation until flux picks the object up
// and patches its status. Every spec change opens it, and a leftover Stalled read
// inside it tells the user the work has been abandoned while it is in fact about to
// begin.
//
// A generation newer than the object's fails the same test. Nothing is known to
// produce one — spec and status travel together in one snapshot — but a verdict
// about a generation this pass is not holding is not a verdict about it either way.
//
// A condition with no observedGeneration is taken as current. Both controllers stamp
// every condition they write through their patch helper, so an unstamped one is not
// a stale verdict but a verdict with no generation recorded, and dropping it would
// hide a real fault — including a terminal one, which nothing would then bring back.
func currentCondition(conditions []metav1.Condition, conditionType string, generation int64) *metav1.Condition {
	cond := apimeta.FindStatusCondition(conditions, conditionType)
	if cond == nil || (cond.ObservedGeneration != 0 && cond.ObservedGeneration != generation) {
		return nil
	}

	return cond
}
