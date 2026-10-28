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

// The message helm-controller reports while it keeps retrying a release whose own
// ServiceAccount the API server refuses.
const forbiddenCause = "Helm install failed for release delivery/hap-test-e0c69c93ec8e with chart " +
	"crd-test@0.1.0+94f7c2be863c: failed to apply CustomResourceDefinitions: failed to create " +
	"CustomResourceDefinition widgets.example.flant.com: server-side apply failed for object " +
	"/widgets.example.flant.com apiextensions.k8s.io/v1, Kind=CustomResourceDefinition: " +
	"customresourcedefinitions.apiextensions.k8s.io \"widgets.example.flant.com\" is forbidden: " +
	"User \"system:serviceaccount:d8-operator-helm:hap-delivery-test-72660c37044f\" cannot patch " +
	"resource \"customresourcedefinitions\" in API group \"apiextensions.k8s.io\" at the cluster scope"

func TestReduceInternalConditionsStallsOnATerminalMessageUnderReconciling(t *testing.T) {
	state := reduceInternalConditions([]metav1.Condition{
		{
			Type:               helmv1alpha1.ConditionTypeReconciling,
			Status:             metav1.ConditionTrue,
			Reason:             "Progressing",
			Message:            "Running 'install' action with timeout of 5m0s",
			ObservedGeneration: 1,
		},
		{
			Type:               helmv1alpha1.ConditionTypeReady,
			Status:             metav1.ConditionFalse,
			Reason:             "InstallFailed",
			Message:            forbiddenCause,
			ObservedGeneration: 1,
		},
		{
			Type:               "Released",
			Status:             metav1.ConditionFalse,
			Reason:             "InstallFailed",
			Message:            forbiddenCause,
			ObservedGeneration: 1,
		},
	}, 1, helmReleaseErrorRules)

	if !state.Observed || !state.Stalled || state.Status != metav1.ConditionFalse {
		t.Fatalf("state = %+v, want an observed, stalled failure rather than work in flight", state)
	}
	if state.Reason != helmv1alpha1.ReasonAccessDenied {
		t.Fatalf("reason = %q, want %q", state.Reason, helmv1alpha1.ReasonAccessDenied)
	}
	if state.Message != forbiddenCause {
		t.Fatalf("message = %q, want the fault the internal object reported", state.Message)
	}
}

func TestReduceInternalConditionsIgnoresATerminalMessageInAnotherCondition(t *testing.T) {
	state := reduceInternalConditions([]metav1.Condition{
		{
			Type:               helmv1alpha1.ConditionTypeReconciling,
			Status:             metav1.ConditionTrue,
			Reason:             "Progressing",
			Message:            forbiddenCause,
			ObservedGeneration: 1,
		},
	}, 1, helmReleaseErrorRules)

	if state.Stalled {
		t.Fatalf("state = %+v, want no rule to match a condition type it does not name", state)
	}
}

func TestReduceInternalConditionsIgnoresATerminalMessageFromAnOlderGeneration(t *testing.T) {
	state := reduceInternalConditions([]metav1.Condition{
		{
			Type:               helmv1alpha1.ConditionTypeReconciling,
			Status:             metav1.ConditionTrue,
			Reason:             "Progressing",
			Message:            "Running 'install' action with timeout of 5m0s",
			ObservedGeneration: 2,
		},
		{
			Type:               helmv1alpha1.ConditionTypeReady,
			Status:             metav1.ConditionFalse,
			Reason:             "InstallFailed",
			Message:            forbiddenCause,
			ObservedGeneration: 1,
		},
	}, 2, helmReleaseErrorRules)

	if state.Stalled {
		t.Fatalf("state = %+v, want the refusal of the previous generation ignored", state)
	}
	if state.Observed || state.Reason != helmv1alpha1.ReasonReconciling {
		t.Fatalf("state = %+v, want work in flight", state)
	}
}

func TestReduceInternalConditionsStallsOnAChartThatDoesNotRender(t *testing.T) {
	const cause = "Helm install failed for release demo/html with chart example-html@21.3.278: " +
		"error while running post render on files: map[string]interface {}(nil): yaml: unmarshal errors:\n" +
		"  line 21: mapping key \"app.startx.fr/component\" already defined at line 13"

	state := reduceInternalConditions([]metav1.Condition{
		{
			Type:               helmv1alpha1.ConditionTypeReconciling,
			Status:             metav1.ConditionTrue,
			Reason:             "Progressing",
			Message:            "Running 'install' action with timeout of 5m0s",
			ObservedGeneration: 1,
		},
		{
			Type:               helmv1alpha1.ConditionTypeReady,
			Status:             metav1.ConditionFalse,
			Reason:             "InstallFailed",
			Message:            cause,
			ObservedGeneration: 1,
		},
		{
			Type:               "Released",
			Status:             metav1.ConditionFalse,
			Reason:             "InstallFailed",
			Message:            cause,
			ObservedGeneration: 1,
		},
	}, 1, helmReleaseErrorRules)

	if !state.Observed || !state.Stalled || state.Status != metav1.ConditionFalse {
		t.Fatalf("state = %+v, want an observed, stalled failure rather than work in flight", state)
	}
	if state.Reason != helmv1alpha1.ReasonRenderFailed {
		t.Fatalf("reason = %q, want %q", state.Reason, helmv1alpha1.ReasonRenderFailed)
	}
	if state.Message != cause {
		t.Fatalf("message = %q, want the fault the internal object reported", state.Message)
	}
}

const artifactCause = "Could not load chart: GET http://source-controller.d8-operator-helm.svc/ocirepository/" +
	"d8-operator-helm/hap-delivery-podinfo/sha256:258d.tar.gz giving up after 10 attempt(s): " +
	"dial tcp 10.222.50.77:80: connect: connection refused"

func prerequisitesConditions(ready metav1.Condition, extra ...metav1.Condition) []metav1.Condition {
	ready.Type = helmv1alpha1.ConditionTypeReady
	ready.ObservedGeneration = 3

	return append([]metav1.Condition{
		{
			Type:               helmv1alpha1.ConditionTypeReconciling,
			Status:             metav1.ConditionTrue,
			Reason:             "Progressing",
			Message:            "Fulfilling prerequisites",
			ObservedGeneration: 3,
		},
		ready,
	}, extra...)
}

func TestReduceInternalConditionsReportsAFailedPrerequisite(t *testing.T) {
	state := reduceInternalConditions(prerequisitesConditions(
		metav1.Condition{Status: metav1.ConditionFalse, Reason: "ArtifactFailed", Message: artifactCause},
		metav1.Condition{
			Type:               "Released",
			Status:             metav1.ConditionTrue,
			Reason:             "UpgradeSucceeded",
			Message:            "Helm upgrade succeeded",
			ObservedGeneration: 3,
		},
	), 3, helmReleaseErrorRules)

	if !state.Observed || state.Status != metav1.ConditionFalse || state.Reason != helmv1alpha1.ReasonChartFetchFailed {
		t.Fatalf("state = %+v, want an observed False/%s rather than work in flight", state, helmv1alpha1.ReasonChartFetchFailed)
	}
	if state.Stalled {
		t.Fatalf("state = %+v, want a failed prerequisite left retriable", state)
	}
	if state.Message != artifactCause {
		t.Fatalf("message = %q, want the fault helm-controller reported", state.Message)
	}
}

func TestReduceInternalConditionsPrefersAFailedPrerequisiteToAStaleRelease(t *testing.T) {
	state := reduceInternalConditions(prerequisitesConditions(
		metav1.Condition{Status: metav1.ConditionFalse, Reason: "ArtifactFailed", Message: artifactCause},
		metav1.Condition{
			Type:               "Released",
			Status:             metav1.ConditionFalse,
			Reason:             "UpgradeFailed",
			Message:            "Helm upgrade failed",
			ObservedGeneration: 3,
		},
	), 3, helmReleaseErrorRules)

	if state.Reason != helmv1alpha1.ReasonChartFetchFailed || state.Message != artifactCause {
		t.Fatalf("state = %+v, want the prerequisite that failed in this pass", state)
	}
}

func TestReduceInternalConditionsKeepsTheVerdictOfThePreviousActionWhilePrerequisitesRun(t *testing.T) {
	state := reduceInternalConditions(prerequisitesConditions(
		metav1.Condition{Status: metav1.ConditionFalse, Reason: "UpgradeFailed", Message: "Helm upgrade failed"},
		metav1.Condition{
			Type:               "Released",
			Status:             metav1.ConditionFalse,
			Reason:             "UpgradeFailed",
			Message:            "Helm upgrade failed",
			ObservedGeneration: 3,
		},
	), 3, helmReleaseErrorRules)

	if !state.Observed || state.Status != metav1.ConditionFalse || state.Reason != helmv1alpha1.ReasonReleaseFailed {
		t.Fatalf("state = %+v, want the False/%s the failed action left", state, helmv1alpha1.ReasonReleaseFailed)
	}
}

func TestReduceInternalConditionsKeepsPrerequisitesOfANewGenerationInFlight(t *testing.T) {
	conditions := prerequisitesConditions(
		metav1.Condition{Status: metav1.ConditionFalse, Reason: "ArtifactFailed", Message: artifactCause},
	)
	conditions[1].ObservedGeneration = 2

	state := reduceInternalConditions(conditions, 3, helmReleaseErrorRules)

	if state.Observed || state.Status != metav1.ConditionUnknown || state.Reason != helmv1alpha1.ReasonReconciling {
		t.Fatalf("state = %+v, want work in flight rather than the previous generation's failure", state)
	}
}

func TestReduceInternalConditionsStallsOnATerminalMessageInAFailedPrerequisite(t *testing.T) {
	state := reduceInternalConditions(prerequisitesConditions(
		metav1.Condition{Status: metav1.ConditionFalse, Reason: "StateError", Message: forbiddenCause},
	), 3, helmReleaseErrorRules)

	if !state.Stalled || state.Reason != helmv1alpha1.ReasonAccessDenied {
		t.Fatalf("state = %+v, want a stalled %s", state, helmv1alpha1.ReasonAccessDenied)
	}
}

func TestReduceInternalConditionsKeepsARunningActionInFlight(t *testing.T) {
	state := reduceInternalConditions([]metav1.Condition{
		{
			Type:               helmv1alpha1.ConditionTypeReconciling,
			Status:             metav1.ConditionTrue,
			Reason:             "Progressing",
			Message:            "Running 'rollback' action with timeout of 5m0s",
			ObservedGeneration: 3,
		},
		{
			Type:               helmv1alpha1.ConditionTypeReady,
			Status:             metav1.ConditionFalse,
			Reason:             "UpgradeFailed",
			Message:            "Helm upgrade failed",
			ObservedGeneration: 3,
		},
	}, 3, helmReleaseErrorRules)

	if state.Observed || state.Status != metav1.ConditionUnknown || state.Reason != helmv1alpha1.ReasonReconciling {
		t.Fatalf("state = %+v, want work in flight", state)
	}
}

func TestReduceInternalConditionsTranslatesUpstreamReasons(t *testing.T) {
	tests := []struct {
		name        string
		ready       metav1.Condition
		wantStatus  metav1.ConditionStatus
		wantReason  string
		wantMessage string
	}{
		{
			name:        "a fetched artifact that failed",
			ready:       metav1.Condition{Status: metav1.ConditionFalse, Reason: "ArtifactFailed", Message: "artifact not found"},
			wantStatus:  metav1.ConditionFalse,
			wantReason:  helmv1alpha1.ReasonChartFetchFailed,
			wantMessage: "artifact not found",
		},
		{
			name:        "a source that is not ready",
			ready:       metav1.Condition{Status: metav1.ConditionFalse, Reason: "SourceNotReady", Message: "HelmChart is not ready"},
			wantStatus:  metav1.ConditionFalse,
			wantReason:  helmv1alpha1.ReasonChartSourceNotReady,
			wantMessage: "HelmChart is not ready",
		},
		{
			name:        "values that could not be composed",
			ready:       metav1.Condition{Status: metav1.ConditionFalse, Reason: "ValuesError", Message: "no values"},
			wantStatus:  metav1.ConditionFalse,
			wantReason:  helmv1alpha1.ReasonInvalidValues,
			wantMessage: "no values",
		},
		{
			name:        "a reason of its own",
			ready:       metav1.Condition{Status: metav1.ConditionFalse, Reason: "DependencyNotReady", Message: "dependency 'a/b' is not ready"},
			wantStatus:  metav1.ConditionFalse,
			wantReason:  helmv1alpha1.ReasonFailed,
			wantMessage: "DependencyNotReady: dependency 'a/b' is not ready",
		},
		{
			name:        "a reason of its own without a message",
			ready:       metav1.Condition{Status: metav1.ConditionFalse, Reason: "FactoryError"},
			wantStatus:  metav1.ConditionFalse,
			wantReason:  helmv1alpha1.ReasonFailed,
			wantMessage: "FactoryError",
		},
		{
			name:        "a release that succeeded",
			ready:       metav1.Condition{Status: metav1.ConditionTrue, Reason: "UpgradeSucceeded", Message: "Helm upgrade succeeded"},
			wantStatus:  metav1.ConditionTrue,
			wantReason:  "UpgradeSucceeded",
			wantMessage: "Helm upgrade succeeded",
		},
		{
			name:        "a release in progress",
			ready:       metav1.Condition{Status: metav1.ConditionUnknown, Reason: "Progressing", Message: "reconciliation in progress"},
			wantStatus:  metav1.ConditionUnknown,
			wantReason:  helmv1alpha1.ReasonReconciling,
			wantMessage: "reconciliation in progress",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ready := test.ready
			ready.Type = helmv1alpha1.ConditionTypeReady
			ready.ObservedGeneration = 3

			state := reduceInternalConditions([]metav1.Condition{ready}, 3, helmReleaseErrorRules)

			if state.Status != test.wantStatus || state.Reason != test.wantReason || state.Message != test.wantMessage {
				t.Fatalf("state = %s/%s %q, want %s/%s %q",
					state.Status, state.Reason, state.Message, test.wantStatus, test.wantReason, test.wantMessage)
			}
		})
	}
}

func TestReduceInternalConditionsFallsBackToAStallReasonOfItsOwn(t *testing.T) {
	state := reduceInternalConditions([]metav1.Condition{
		{
			Type:               helmv1alpha1.ConditionTypeStalled,
			Status:             metav1.ConditionTrue,
			Reason:             "MissingRollbackTarget",
			Message:            "Failed to perform remediation: no previous release",
			ObservedGeneration: 7,
		},
	}, 7, helmReleaseErrorRules)

	if !state.Stalled || state.Reason != helmv1alpha1.ReasonFailed {
		t.Fatalf("state = %+v, want a stalled %s", state, helmv1alpha1.ReasonFailed)
	}
	if state.Message != "MissingRollbackTarget: Failed to perform remediation: no previous release" {
		t.Fatalf("message = %q, want the upstream reason ahead of its message", state.Message)
	}
}
