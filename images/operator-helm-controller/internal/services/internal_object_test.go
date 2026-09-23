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
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
)

// TestReduceInternalConditionsCarriesGivingUp reproduces an internal release that
// spent its remediation attempts on values the chart cannot render. The reduced
// state has to say two things at once: what the fault was, which only the error
// rules know, and that nothing more is coming, which only the Stalled condition
// knows. Losing the second is what lets a release report a retry that never runs.
func TestReduceInternalConditionsCarriesGivingUp(t *testing.T) {
	const cause = "Helm upgrade failed for release default/podinfo-app-helm: " +
		".spec.replicas: expected numeric (int or float), got string"

	state := reduceInternalConditions([]metav1.Condition{
		{
			Type:               helmv1alpha1.ConditionTypeStalled,
			Status:             metav1.ConditionTrue,
			Reason:             helmv1alpha1.ReasonRetriesExceeded,
			Message:            "Failed to upgrade after 1 attempt(s)",
			ObservedGeneration: 4,
		},
		{
			Type:               helmv1alpha1.ConditionTypeReady,
			Status:             metav1.ConditionFalse,
			Reason:             "UpgradeFailed",
			Message:            cause,
			ObservedGeneration: 4,
		},
		{
			Type:               "Released",
			Status:             metav1.ConditionFalse,
			Reason:             "UpgradeFailed",
			Message:            cause,
			ObservedGeneration: 4,
		},
	}, 4, helmReleaseErrorRules)

	if !state.Stalled {
		t.Fatalf("state = %+v, want it to carry that the object gave up", state)
	}
	if state.Status != metav1.ConditionFalse || state.Reason != helmv1alpha1.ReasonReleaseFailed {
		t.Fatalf("state = %s/%s, want False/%s", state.Status, state.Reason, helmv1alpha1.ReasonReleaseFailed)
	}
	if state.Message != cause {
		t.Fatalf("message = %q, want the fault the error rule named", state.Message)
	}
}

// TestReduceInternalConditionsIgnoresProgressOfAnObjectThatGaveUp pins the one
// ordering that matters. A stalled object can still carry the Reconciling condition
// of the attempt that gave up; read as work in flight it would hide the failure
// behind Unknown and keep the release waiting for a verdict that never comes.
func TestReduceInternalConditionsIgnoresProgressOfAnObjectThatGaveUp(t *testing.T) {
	state := reduceInternalConditions([]metav1.Condition{
		{
			Type:               helmv1alpha1.ConditionTypeStalled,
			Status:             metav1.ConditionTrue,
			Reason:             helmv1alpha1.ReasonRetriesExceeded,
			Message:            "Failed to upgrade after 1 attempt(s)",
			ObservedGeneration: 2,
		},
		{
			Type:               helmv1alpha1.ConditionTypeReconciling,
			Status:             metav1.ConditionTrue,
			Reason:             helmv1alpha1.ReasonReconciling,
			Message:            "Running 'upgrade' action",
			ObservedGeneration: 2,
		},
		{
			Type:               "Released",
			Status:             metav1.ConditionFalse,
			Reason:             "UpgradeFailed",
			Message:            "Helm upgrade failed",
			ObservedGeneration: 2,
		},
	}, 2, helmReleaseErrorRules)

	if state.Status != metav1.ConditionFalse {
		t.Fatalf("state = %+v, want the failure rather than work in flight", state)
	}
	if !state.Stalled {
		t.Fatalf("state = %+v, want it to carry that the object gave up", state)
	}
}

// TestReduceInternalConditionsFallsBackToTheStallReason covers a stalled object no
// error rule matches and whose Ready is about an older spec: its own account of why
// it stopped is then the only verdict there is.
func TestReduceInternalConditionsFallsBackToTheStallReason(t *testing.T) {
	state := reduceInternalConditions([]metav1.Condition{
		{
			Type:               helmv1alpha1.ConditionTypeStalled,
			Status:             metav1.ConditionTrue,
			Reason:             helmv1alpha1.ReasonRetriesExceeded,
			Message:            "Failed to install after 3 attempt(s)",
			ObservedGeneration: 7,
		},
		{
			Type:               helmv1alpha1.ConditionTypeReady,
			Status:             metav1.ConditionTrue,
			Reason:             "InstallSucceeded",
			ObservedGeneration: 6,
		},
	}, 7, helmReleaseErrorRules)

	if !state.Observed || !state.Stalled || state.Status != metav1.ConditionFalse {
		t.Fatalf("state = %+v, want an observed, stalled failure", state)
	}
	if state.Reason != helmv1alpha1.ReasonRetriesExceeded {
		t.Fatalf("reason = %q, want %q", state.Reason, helmv1alpha1.ReasonRetriesExceeded)
	}
}

// TestReduceInternalConditionsLeavesAnOrdinaryFailureRetriable is the counterpart:
// the same failure without a Stalled condition is one the object still means to
// retry, and the release must go on reporting progress for it.
func TestReduceInternalConditionsLeavesAnOrdinaryFailureRetriable(t *testing.T) {
	state := reduceInternalConditions([]metav1.Condition{
		{
			Type:               helmv1alpha1.ConditionTypeReady,
			Status:             metav1.ConditionFalse,
			Reason:             "UpgradeFailed",
			Message:            "Helm upgrade failed",
			ObservedGeneration: 3,
		},
		{
			Type:               "Released",
			Status:             metav1.ConditionFalse,
			Reason:             "UpgradeFailed",
			Message:            "Helm upgrade failed",
			ObservedGeneration: 3,
		},
	}, 3, helmReleaseErrorRules)

	if state.Stalled {
		t.Fatalf("state = %+v, want it left retriable", state)
	}
	if state.Reason != helmv1alpha1.ReasonReleaseFailed {
		t.Fatalf("reason = %q, want %q", state.Reason, helmv1alpha1.ReasonReleaseFailed)
	}
}
