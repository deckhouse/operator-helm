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

// TestReduceInternalConditionsIgnoresAStallFromAnOlderGeneration covers the moment
// right after a broken spec is corrected: the generation has been bumped, and the
// pass that will clear Stalled has not started yet. helm-controller drops that
// condition at the head of its next pass, so until then the status still carries the
// verdict of the attempt that gave up on the previous spec. Read as a verdict about
// the current one it would suppress Reconciling and tell the user that nothing is
// happening, right when the work is about to begin.
func TestReduceInternalConditionsIgnoresAStallFromAnOlderGeneration(t *testing.T) {
	state := reduceInternalConditions([]metav1.Condition{
		{
			Type:               helmv1alpha1.ConditionTypeStalled,
			Status:             metav1.ConditionTrue,
			Reason:             helmv1alpha1.ReasonRetriesExceeded,
			Message:            "Failed to upgrade after 1 attempt(s)",
			ObservedGeneration: 4,
		},
		{
			Type:               helmv1alpha1.ConditionTypeReconciling,
			Status:             metav1.ConditionTrue,
			Reason:             helmv1alpha1.ReasonReconciling,
			Message:            "Running 'upgrade' action",
			ObservedGeneration: 5,
		},
		{
			Type:               "Released",
			Status:             metav1.ConditionFalse,
			Reason:             "UpgradeFailed",
			Message:            "Helm upgrade failed",
			ObservedGeneration: 4,
		},
	}, 5, helmReleaseErrorRules)

	if state.Stalled {
		t.Fatalf("state = %+v, want the stall of the previous generation ignored", state)
	}
	if state.Observed || state.Status != metav1.ConditionUnknown || state.Reason != helmv1alpha1.ReasonReconciling {
		t.Fatalf("state = %+v, want work in flight", state)
	}
}

// TestReduceInternalConditionsIgnoresAFailureFromAnOlderGeneration extends the same
// rule to the error conditions. An object that has been given a new spec and has not
// been picked up yet carries only the fault of the previous one; reporting that fault
// as the verdict about the new spec would blame it for something it never caused.
func TestReduceInternalConditionsIgnoresAFailureFromAnOlderGeneration(t *testing.T) {
	state := reduceInternalConditions([]metav1.Condition{
		{
			Type:               helmv1alpha1.ConditionTypeReady,
			Status:             metav1.ConditionFalse,
			Reason:             "UpgradeFailed",
			Message:            "Helm upgrade failed",
			ObservedGeneration: 2,
		},
		{
			Type:               "Released",
			Status:             metav1.ConditionFalse,
			Reason:             "UpgradeFailed",
			Message:            "Helm upgrade failed",
			ObservedGeneration: 2,
		},
	}, 3, helmReleaseErrorRules)

	if state.Observed || state.Status != metav1.ConditionUnknown || state.Reason != helmv1alpha1.ReasonReconciling {
		t.Fatalf("state = %+v, want no verdict yet for the current spec", state)
	}
}

// TestReduceInternalConditionsTrustsAStallWithoutAnObservedGeneration pins how an
// unstamped condition is read. Both controllers stamp every condition they write, so
// a missing observedGeneration is not evidence of a stale verdict — and discarding
// it would leave a terminal failure reported as work in flight forever, since
// nothing on this side ever revisits it.
func TestReduceInternalConditionsTrustsAStallWithoutAnObservedGeneration(t *testing.T) {
	state := reduceInternalConditions([]metav1.Condition{
		{
			Type:    helmv1alpha1.ConditionTypeStalled,
			Status:  metav1.ConditionTrue,
			Reason:  helmv1alpha1.ReasonRetriesExceeded,
			Message: "Failed to install after 3 attempt(s)",
		},
		{
			Type:               helmv1alpha1.ConditionTypeReconciling,
			Status:             metav1.ConditionTrue,
			Reason:             helmv1alpha1.ReasonReconciling,
			Message:            "Running 'install' action",
			ObservedGeneration: 9,
		},
	}, 9, helmReleaseErrorRules)

	if !state.Observed || !state.Stalled || state.Status != metav1.ConditionFalse {
		t.Fatalf("state = %+v, want an observed, stalled failure", state)
	}
	if state.Reason != helmv1alpha1.ReasonRetriesExceeded {
		t.Fatalf("reason = %q, want %q", state.Reason, helmv1alpha1.ReasonRetriesExceeded)
	}
}

// TestReduceInternalConditionsIgnoresAVerdictFromANewerGeneration covers the other
// end of the same gap. flux patches a status the instant it starts a pass, and that
// status can reach this cache before the spec it was written for: the conditions
// then carry a generation ahead of the one read here. They are no more about the
// spec in hand than an older verdict is, so nothing is reported as observed until
// the two line up.
func TestReduceInternalConditionsIgnoresAVerdictFromANewerGeneration(t *testing.T) {
	state := reduceInternalConditions([]metav1.Condition{
		{
			Type:               helmv1alpha1.ConditionTypeStalled,
			Status:             metav1.ConditionTrue,
			Reason:             helmv1alpha1.ReasonRetriesExceeded,
			Message:            "Failed to upgrade after 1 attempt(s)",
			ObservedGeneration: 6,
		},
		{
			Type:               helmv1alpha1.ConditionTypeReady,
			Status:             metav1.ConditionFalse,
			Reason:             "UpgradeFailed",
			Message:            "Helm upgrade failed",
			ObservedGeneration: 6,
		},
		{
			Type:               "Released",
			Status:             metav1.ConditionFalse,
			Reason:             "UpgradeFailed",
			Message:            "Helm upgrade failed",
			ObservedGeneration: 6,
		},
	}, 5, helmReleaseErrorRules)

	if state.Stalled {
		t.Fatalf("state = %+v, want a stall of a generation not yet seen ignored", state)
	}
	if state.Observed || state.Status != metav1.ConditionUnknown || state.Reason != helmv1alpha1.ReasonReconciling {
		t.Fatalf("state = %+v, want no verdict yet for the generation in hand", state)
	}
}

// TestReduceInternalConditionsPrefersACurrentFailureToAStaleReconciling is the
// mirror of the stale-stall case: here it is Reconciling that belongs to the
// previous generation while the failure is about the current one. Reconciling comes
// first in the reduction, so were it read the release would report progress over a
// fault the object has already settled on.
func TestReduceInternalConditionsPrefersACurrentFailureToAStaleReconciling(t *testing.T) {
	state := reduceInternalConditions([]metav1.Condition{
		{
			Type:               helmv1alpha1.ConditionTypeReconciling,
			Status:             metav1.ConditionTrue,
			Reason:             helmv1alpha1.ReasonReconciling,
			Message:            "Running 'upgrade' action",
			ObservedGeneration: 4,
		},
		{
			Type:               helmv1alpha1.ConditionTypeReady,
			Status:             metav1.ConditionFalse,
			Reason:             "UpgradeFailed",
			Message:            "Helm upgrade failed",
			ObservedGeneration: 5,
		},
		{
			Type:               "Released",
			Status:             metav1.ConditionFalse,
			Reason:             "UpgradeFailed",
			Message:            "Helm upgrade failed",
			ObservedGeneration: 5,
		},
	}, 5, helmReleaseErrorRules)

	if !state.Observed || state.Status != metav1.ConditionFalse || state.Reason != helmv1alpha1.ReasonReleaseFailed {
		t.Fatalf("state = %+v, want the failure of the current generation", state)
	}
	if state.Stalled {
		t.Fatalf("state = %+v, want it left retriable", state)
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
