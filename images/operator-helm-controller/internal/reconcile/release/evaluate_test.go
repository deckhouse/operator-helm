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

package release

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	helmv2 "github.com/fluxcd/helm-controller/api/v2"
	"github.com/fluxcd/pkg/apis/meta"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
	"github.com/deckhouse/operator-helm/internal/internalobj"
)

func baseInputs() Inputs {
	return Inputs{
		Generation:     2,
		Now:            time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC),
		ConditionTypes: []string{helmv1alpha1.ConditionTypeReady, helmv1alpha1.ConditionTypeInstalled},
	}
}

func condition(t *testing.T, decision Decision, conditionType string) metav1.Condition {
	t.Helper()

	cond := apimeta.FindStatusCondition(decision.Conditions, conditionType)
	if cond == nil {
		t.Fatalf("condition %q was not written, got %+v", conditionType, decision.Conditions)
	}

	return *cond
}

func readyInternal() internalobj.InternalObjectState {
	return internalobj.InternalObjectState{
		Observed: true,
		Status:   metav1.ConditionTrue,
		Reason:   "Succeeded",
		Message:  "stored artifact",
	}
}

func TestEvaluateTerminalFailureStalls(t *testing.T) {
	in := baseInputs()
	failure := Failure{
		Reason:   helmv1alpha1.ReasonFailed,
		Message:  "Target namespace cannot be a system namespace",
		Err:      errors.New("system namespace"),
		Terminal: true,
	}
	in.Outcome = StepFailed{failure}

	decision := Evaluate(in)

	ready := condition(t, decision, helmv1alpha1.ConditionTypeReady)
	if ready.Status != metav1.ConditionFalse || ready.Reason != helmv1alpha1.ReasonFailed {
		t.Fatalf("Ready = %s/%s, want False/%s", ready.Status, ready.Reason, helmv1alpha1.ReasonFailed)
	}

	stalled := condition(t, decision, helmv1alpha1.ConditionTypeStalled)
	if stalled.Status != metav1.ConditionTrue || stalled.Message != failure.Message {
		t.Fatalf("Stalled = %s/%q, want True carrying the cause", stalled.Status, stalled.Message)
	}
	if slices.Contains(decision.RemoveConditions, helmv1alpha1.ConditionTypeStalled) {
		t.Fatal("Stalled must not be both written and removed")
	}
	if decision.Err != nil {
		t.Fatalf("Err = %v, want none: retrying cannot change the cause", decision.Err)
	}
}

func TestEvaluateTerminalFailureIsNeverRetried(t *testing.T) {
	in := baseInputs()
	in.Outcome = StepFailed{Failure{
		Reason:   helmv1alpha1.ReasonForeignRBACObject,
		Message:  "role team-a/operator-helm-application already exists and is not managed by the operator",
		Err:      errors.New("foreign role"),
		Terminal: true,
	}}

	decision := Evaluate(in)

	if decision.Err != nil {
		t.Fatalf("Err = %v, want none", decision.Err)
	}
	condition(t, decision, helmv1alpha1.ConditionTypeStalled)
}

func TestEvaluateTerminalFailureDropsTheForceRequest(t *testing.T) {
	in := baseInputs()
	in.Outcome = StepFailed{Failure{
		Reason:   helmv1alpha1.ReasonForeignRBACObject,
		Message:  "role binding team-a/app already exists and is not managed by the operator",
		Err:      errors.New("foreign binding"),
		Terminal: true,
	}}

	decision := Evaluate(in)

	if !decision.ConsumeForce {
		t.Fatal("a terminal failure must drop the force request: no later pass will honour it")
	}
	if decision.ForceReconcileTime != nil {
		t.Fatalf("ForceReconcileTime = %v, want none: the request was dropped, not acted on", decision.ForceReconcileTime)
	}
	if !slices.Contains(decision.RemoveConditions, helmv1alpha1.ConditionTypeReconciling) {
		t.Fatal("Reconciling must be taken away: no work is in flight behind a terminal verdict")
	}
}

func TestEvaluateRecoverableFailureKeepsTheForceRequest(t *testing.T) {
	in := baseInputs()
	in.Outcome = StepFailed{Failure{
		Reason:  helmv1alpha1.ReasonRBACSetupFailed,
		Message: "Failed to set up the release identity",
		Err:     errors.New("forbidden"),
	}}

	if decision := Evaluate(in); decision.ConsumeForce {
		t.Fatal("a recoverable failure must leave the force request for the pass that can honour it")
	}
}

func TestEvaluateTerminalProbeVerdictStalls(t *testing.T) {
	in := baseInputs()
	probe := internalobj.OCIRepoOutcome{
		ProbeErr:      errors.New("unauthorized"),
		ProbeReason:   helmv1alpha1.ReasonAuthenticationFailed,
		ProbeMessage:  "repository oci://example.com/charts rejected the credentials (HTTP 401)",
		ProbeTerminal: true,
	}
	in.Outcome = ChartReached{Internal: ServedByOCIRepository{Result: probe}}

	decision := Evaluate(in)

	ready := condition(t, decision, helmv1alpha1.ConditionTypeReady)
	if ready.Status != metav1.ConditionFalse || ready.Reason != helmv1alpha1.ReasonAuthenticationFailed {
		t.Fatalf("Ready = %s/%s, want False/%s", ready.Status, ready.Reason, helmv1alpha1.ReasonAuthenticationFailed)
	}

	stalled := condition(t, decision, helmv1alpha1.ConditionTypeStalled)
	if stalled.Status != metav1.ConditionTrue || stalled.Message != probe.ProbeMessage {
		t.Fatalf("Stalled = %s/%q, want True carrying the probe verdict", stalled.Status, stalled.Message)
	}
	if slices.Contains(decision.RemoveConditions, helmv1alpha1.ConditionTypeStalled) {
		t.Fatal("Stalled must not be both written and removed")
	}
}

func TestEvaluateRecoverableProbeFailureDoesNotStall(t *testing.T) {
	in := baseInputs()
	in.Outcome = ChartReached{
		Internal: ServedByOCIRepository{Result: internalobj.OCIRepoOutcome{
			ProbeErr:          errors.New("connection refused"),
			ProbeReason:       helmv1alpha1.ReasonOCIFetchFailed,
			ProbeMessage:      "Failed to examine the chart artifact: connection refused",
			ProbeRequeueAfter: time.Minute,
		}},
	}

	decision := Evaluate(in)

	if !slices.Contains(decision.RemoveConditions, helmv1alpha1.ConditionTypeStalled) {
		t.Fatal("a recoverable probe failure must take Stalled away")
	}
	if decision.RequeueAfter != time.Minute {
		t.Fatalf("RequeueAfter = %v, want the probe's own timer", decision.RequeueAfter)
	}
}

func TestEvaluateStepFailureOwnsReadyAlone(t *testing.T) {
	in := baseInputs()
	failure := Failure{
		Reason:  helmv1alpha1.ReasonRBACSetupFailed,
		Message: "Failed to set up the release identity",
		Err:     errors.New("forbidden"),
	}
	in.Outcome = StepFailed{failure}

	decision := Evaluate(in)

	condition(t, decision, helmv1alpha1.ConditionTypeReady)
	for _, conditionType := range []string{
		helmv1alpha1.ConditionTypeInstalled,
		helmv1alpha1.ConditionTypeUpdateInstalled,
		helmv1alpha1.ConditionTypeConfigurationApplied,
	} {
		if apimeta.FindStatusCondition(decision.Conditions, conditionType) != nil {
			t.Fatalf("%s must not be written by a pass that never reached the chart", conditionType)
		}
	}

	progress := condition(t, decision, helmv1alpha1.ConditionTypeReconciling)
	if progress.Status != metav1.ConditionTrue || progress.Reason != helmv1alpha1.ReasonProgressingWithRetry {
		t.Fatalf("Reconciling = %s/%s, want True/%s",
			progress.Status, progress.Reason, helmv1alpha1.ReasonProgressingWithRetry)
	}
	if slices.Contains(decision.RemoveConditions, helmv1alpha1.ConditionTypeReconciling) {
		t.Fatal("Reconciling must not be both written and removed")
	}

	if !slices.Contains(decision.RemoveConditions, helmv1alpha1.ConditionTypeStalled) {
		t.Fatal("a recoverable failure must take Stalled away")
	}
	if !errors.Is(decision.Err, failure.Err) {
		t.Fatalf("Err = %v, want the step's error handed to the work queue", decision.Err)
	}
}

func TestEvaluateStepFailureAwaitingAWatchPromisesNoRetry(t *testing.T) {
	in := baseInputs()
	in.Outcome = StepFailed{Failure{
		Reason:      helmv1alpha1.ReasonFailed,
		Message:     "Failed to get internal repository",
		Err:         errors.New(`getting internal repository: helmapplicationrepositories.helm.deckhouse.io "example" not found`),
		AwaitsWatch: true,
	}}

	decision := Evaluate(in)

	progress := condition(t, decision, helmv1alpha1.ConditionTypeReconciling)
	if progress.Status != metav1.ConditionTrue || progress.Reason != helmv1alpha1.ReasonReconciling {
		t.Fatalf("Reconciling = %s/%s, want True/%s: nothing is being retried",
			progress.Status, progress.Reason, helmv1alpha1.ReasonReconciling)
	}
	if decision.Err != nil {
		t.Fatalf("Err = %v, want none: a watch brings the release back", decision.Err)
	}
}

func TestEvaluateUnresolvedSourceAwaitingASyncPromisesNoRetry(t *testing.T) {
	tests := []struct {
		name  string
		cause error
	}{
		{name: "the catalog of the chart is not written yet", cause: errCatalogAwaited},
		{name: "the repository has reached no verdict on the version", cause: errVersionResolvePending},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := baseInputs()
			in.Outcome = ChartReached{Internal: SourceUnresolved{Failure: Failure{
				Reason:      helmv1alpha1.ReasonChartFetchFailed,
				Message:     "Failed to resolve the desired chart version",
				Err:         tt.cause,
				AwaitsWatch: true,
				RetryAfter:  awaitSyncInterval,
			}}}

			decision := Evaluate(in)

			progress := condition(t, decision, helmv1alpha1.ConditionTypeReconciling)
			if progress.Status != metav1.ConditionTrue || progress.Reason != helmv1alpha1.ReasonReconciling {
				t.Fatalf("Reconciling = %s/%s, want True/%s: nothing is being retried",
					progress.Status, progress.Reason, helmv1alpha1.ReasonReconciling)
			}
			if progress.Message != "Waiting for the repository to resolve the chart version" {
				t.Fatalf("Reconciling message = %q, want one naming the wait rather than a retry", progress.Message)
			}
			if decision.Err != nil {
				t.Fatalf("Err = %v, want none: the wait is expected and rides the timer", decision.Err)
			}
			if decision.RequeueAfter != awaitSyncInterval {
				t.Fatalf("RequeueAfter = %v, want %v: without it the release waits for an event nobody sends",
					decision.RequeueAfter, awaitSyncInterval)
			}
		})
	}
}

func TestEvaluateReportsWorkInFlight(t *testing.T) {
	in := baseInputs()
	in.Outcome = ChartReached{
		Internal: ServedByHelmChart{Result: internalobj.ChartOutcome{
			Artifact: &meta.Artifact{Revision: "6.7.1"},
			Internal: internalobj.InternalObjectState{
				Status:  metav1.ConditionUnknown,
				Reason:  helmv1alpha1.ReasonReconciling,
				Message: "pulling the chart",
			},
		}},
	}

	decision := Evaluate(in)

	progress := condition(t, decision, helmv1alpha1.ConditionTypeReconciling)
	if progress.Status != metav1.ConditionTrue || progress.Reason != helmv1alpha1.ReasonReconciling {
		t.Fatalf("Reconciling = %s/%s, want True/%s",
			progress.Status, progress.Reason, helmv1alpha1.ReasonReconciling)
	}
	if progress.Message != "pulling the chart" {
		t.Fatalf("Reconciling message = %q, want the internal object's own", progress.Message)
	}
	if slices.Contains(decision.RemoveConditions, helmv1alpha1.ConditionTypeReconciling) {
		t.Fatal("Reconciling must not be both written and removed")
	}
}

func TestEvaluateReportsARetryAfterAFailedRollout(t *testing.T) {
	in := baseInputs()
	in.Outcome = ChartReached{
		Internal: ServedByHelmChart{Result: internalobj.ChartOutcome{Artifact: &meta.Artifact{Revision: "6.7.1"}, Internal: readyInternal()}},
		Release: &internalobj.ReleaseOutcome{
			ChartDeployed: true,
			Internal: internalobj.InternalObjectState{
				Observed: true,
				Status:   metav1.ConditionFalse,
				Reason:   helmv1alpha1.ReasonReleaseFailed,
				Message:  "upgrade failed",
			},
		},
	}

	decision := Evaluate(in)

	progress := condition(t, decision, helmv1alpha1.ConditionTypeReconciling)
	if progress.Status != metav1.ConditionTrue || progress.Reason != helmv1alpha1.ReasonProgressingWithRetry {
		t.Fatalf("Reconciling = %s/%s, want True/%s",
			progress.Status, progress.Reason, helmv1alpha1.ReasonProgressingWithRetry)
	}
}

func TestEvaluateGivenUpReleaseStallsInsteadOfReportingProgress(t *testing.T) {
	const cause = "Helm upgrade failed for release default/podinfo: .spec.replicas: expected numeric, got string"

	in := baseInputs()
	in.Outcome = ChartReached{
		Internal: ServedByHelmChart{Result: internalobj.ChartOutcome{Artifact: &meta.Artifact{Revision: "6.15.0"}, Internal: readyInternal()}},
		Release: &internalobj.ReleaseOutcome{
			Internal: internalobj.InternalObjectState{
				Observed: true,
				Stalled:  true,
				Status:   metav1.ConditionFalse,
				Reason:   helmv1alpha1.ReasonReleaseFailed,
				Message:  cause,
			},
		},
	}

	decision := Evaluate(in)

	stalled := condition(t, decision, helmv1alpha1.ConditionTypeStalled)
	if stalled.Status != metav1.ConditionTrue || stalled.Reason != helmv1alpha1.ReasonReleaseFailed {
		t.Fatalf("Stalled = %s/%s, want True/%s",
			stalled.Status, stalled.Reason, helmv1alpha1.ReasonReleaseFailed)
	}
	if stalled.Message != cause {
		t.Fatalf("Stalled message = %q, want the fault the internal object named", stalled.Message)
	}
	if slices.Contains(decision.RemoveConditions, helmv1alpha1.ConditionTypeStalled) {
		t.Fatal("Stalled must not be both written and removed")
	}

	if apimeta.FindStatusCondition(decision.Conditions, helmv1alpha1.ConditionTypeReconciling) != nil {
		t.Fatal("a release whose internal object gave up must not report a retry that is not coming")
	}
	if !slices.Contains(decision.RemoveConditions, helmv1alpha1.ConditionTypeReconciling) {
		t.Fatal("Reconciling must be taken away once the internal object gave up")
	}
}

func TestEvaluateFailingReleaseStillReportsProgress(t *testing.T) {
	in := baseInputs()
	in.Outcome = ChartReached{
		Internal: ServedByHelmChart{Result: internalobj.ChartOutcome{Artifact: &meta.Artifact{Revision: "6.15.0"}, Internal: readyInternal()}},
		Release: &internalobj.ReleaseOutcome{
			Internal: internalobj.InternalObjectState{
				Observed: true,
				Status:   metav1.ConditionFalse,
				Reason:   helmv1alpha1.ReasonReleaseFailed,
				Message:  "Helm upgrade failed",
			},
		},
	}

	decision := Evaluate(in)

	if apimeta.FindStatusCondition(decision.Conditions, helmv1alpha1.ConditionTypeStalled) != nil {
		t.Fatal("a failure the internal object has not given up on must not stall the release")
	}
	progress := condition(t, decision, helmv1alpha1.ConditionTypeReconciling)
	if progress.Reason != helmv1alpha1.ReasonProgressingWithRetry {
		t.Fatalf("Reconciling reason = %q, want %q", progress.Reason, helmv1alpha1.ReasonProgressingWithRetry)
	}
}

func TestEvaluateSettledReleaseReportsNoProgress(t *testing.T) {
	in := baseInputs()
	in.Outcome = ChartReached{
		Internal: ServedByHelmChart{Result: internalobj.ChartOutcome{Artifact: &meta.Artifact{Revision: "6.7.1"}, Internal: readyInternal()}},
		Release:  &internalobj.ReleaseOutcome{ChartDeployed: true, Internal: readyInternal()},
	}

	decision := Evaluate(in)

	if apimeta.FindStatusCondition(decision.Conditions, helmv1alpha1.ConditionTypeReconciling) != nil {
		t.Fatal("a settled release must not report work in flight")
	}
	if !slices.Contains(decision.RemoveConditions, helmv1alpha1.ConditionTypeReconciling) {
		t.Fatal("Reconciling must be taken away once the release settled")
	}
}

func TestEvaluateStalledReleaseReportsNoProgress(t *testing.T) {
	in := baseInputs()
	probe := internalobj.OCIRepoOutcome{
		ProbeErr:      errors.New("unauthorized"),
		ProbeReason:   helmv1alpha1.ReasonAuthenticationFailed,
		ProbeMessage:  "repository oci://example.com/charts rejected the credentials (HTTP 401)",
		ProbeTerminal: true,
	}
	in.Outcome = ChartReached{Internal: ServedByOCIRepository{Result: probe}}

	decision := Evaluate(in)

	condition(t, decision, helmv1alpha1.ConditionTypeStalled)
	if apimeta.FindStatusCondition(decision.Conditions, helmv1alpha1.ConditionTypeReconciling) != nil {
		t.Fatal("a stalled release must not also report work in flight")
	}
	if !slices.Contains(decision.RemoveConditions, helmv1alpha1.ConditionTypeReconciling) {
		t.Fatal("Reconciling must be taken away behind a terminal verdict")
	}
}

func TestEvaluateStepFailureOnATimerIsNotHandedToTheWorkQueue(t *testing.T) {
	in := baseInputs()
	in.Outcome = StepFailed{Failure{
		Reason:       helmv1alpha1.ReasonChartClaimConflict,
		Message:      "chart is already used",
		RequeueAfter: chartClaimConflictRequeueInterval,
	}}

	decision := Evaluate(in)

	if decision.Err != nil {
		t.Fatalf("Err = %v, want none: the recovery rides on the timer", decision.Err)
	}
	if decision.RequeueAfter != chartClaimConflictRequeueInterval {
		t.Fatalf("RequeueAfter = %v, want %v", decision.RequeueAfter, chartClaimConflictRequeueInterval)
	}
}

func TestEvaluateProjectsTheFirstFailingStep(t *testing.T) {
	in := baseInputs()
	in.Outcome = ChartReached{
		Internal: ServedByHelmChart{Result: internalobj.ChartOutcome{Artifact: &meta.Artifact{Revision: "6.7.1"}, Internal: readyInternal()}},
		Release: &internalobj.ReleaseOutcome{
			Internal: internalobj.InternalObjectState{
				Observed: true,
				Status:   metav1.ConditionFalse,
				Reason:   helmv1alpha1.ReasonReleaseFailed,
				Message:  "install retries exhausted",
			},
			ChartDeployed: true,
		},
	}

	decision := Evaluate(in)

	for _, conditionType := range in.ConditionTypes {
		cond := condition(t, decision, conditionType)
		if cond.Status != metav1.ConditionFalse || cond.Reason != helmv1alpha1.ReasonReleaseFailed {
			t.Fatalf("%s = %s/%s, want the failing release's verdict", conditionType, cond.Status, cond.Reason)
		}
		if cond.ObservedGeneration != in.Generation {
			t.Fatalf("%s observedGeneration = %d, want %d", conditionType, cond.ObservedGeneration, in.Generation)
		}
	}
}

func TestEvaluateHoldsAReleaseThatHasNotRolledOut(t *testing.T) {
	in := baseInputs()
	in.Outcome = ChartReached{
		Internal:          ServedByHelmChart{Result: internalobj.ChartOutcome{Artifact: &meta.Artifact{Revision: "6.7.2"}, Internal: readyInternal()}},
		Release:           &internalobj.ReleaseOutcome{Internal: readyInternal(), ChartDeployed: false},
		ChartInfoOutdated: true,
	}

	decision := Evaluate(in)

	ready := condition(t, decision, helmv1alpha1.ConditionTypeReady)
	if ready.Status != metav1.ConditionUnknown || ready.Reason != helmv1alpha1.ReasonReconciling {
		t.Fatalf("Ready = %s/%s, want Unknown/%s", ready.Status, ready.Reason, helmv1alpha1.ReasonReconciling)
	}
	if decision.ApplyChart {
		t.Fatal("the record of what was applied must not advance ahead of the rollout")
	}
}

func TestEvaluateRecordsWhatWasApplied(t *testing.T) {
	in := baseInputs()
	in.Outcome = ChartReached{
		Internal: ServedByHelmChart{Result: internalobj.ChartOutcome{Artifact: &meta.Artifact{Revision: "6.7.1"}, Internal: readyInternal()}},
		Release: &internalobj.ReleaseOutcome{
			Internal:      readyInternal(),
			ChartDeployed: true,
			History:       helmv2.Snapshots{{Status: "deployed", ConfigDigest: "sha256:values"}},
		},
		ChartInfoOutdated: true,
		ValuesDigest:      "sha256:values",
	}

	decision := Evaluate(in)

	if !decision.ApplyChart {
		t.Fatal("a rolled-out chart must be recorded as applied")
	}
	if !decision.ApplyValues {
		t.Fatal("values the deployed revision was installed with must be recorded as applied")
	}

	reached := in.Outcome.(ChartReached)
	reached.ValuesDigest = "sha256:other"
	in.Outcome = reached

	if Evaluate(in).ApplyValues {
		t.Fatal("values the deployed revision was not installed with must not be recorded")
	}
}

func TestEvaluateNamesARemovedVersion(t *testing.T) {
	in := baseInputs()
	in.Outcome = ChartReached{
		Internal: ServedByOCIRepository{Result: internalobj.OCIRepoOutcome{
			Internal: internalobj.InternalObjectState{
				Observed: true,
				Status:   metav1.ConditionFalse,
				Reason:   "ArtifactPullFailed",
				Message:  "not found",
			},
			VersionRemoved: true,
			Version:        "6.7.1",
			RepositoryName: "stable",
		}},
	}

	cond := condition(t, Evaluate(in), helmv1alpha1.ConditionTypeReady)
	if cond.Reason != helmv1alpha1.ReasonChartVersionRemoved {
		t.Fatalf("reason = %q, want %q", cond.Reason, helmv1alpha1.ReasonChartVersionRemoved)
	}

	reached := in.Outcome.(ChartReached)
	served := reached.Internal.(ServedByOCIRepository)
	served.Result.Internal = readyInternal()
	reached.Internal = served
	in.Outcome = reached

	cond = condition(t, Evaluate(in), helmv1alpha1.ConditionTypeReady)
	if cond.Reason != "Succeeded" {
		t.Fatalf("reason = %q, want the healthy child's own reason untouched", cond.Reason)
	}
}

func TestEvaluateForcedPassStampsAndConsumesTheRequest(t *testing.T) {
	in := baseInputs()
	in.Outcome = ChartReached{
		Forced:   true,
		Internal: ServedByHelmChart{Result: internalobj.ChartOutcome{Artifact: &meta.Artifact{Revision: "6.7.1"}, Internal: readyInternal()}},
	}

	decision := Evaluate(in)

	if decision.ForceReconcileTime == nil || !decision.ForceReconcileTime.Time.Equal(in.Now) {
		t.Fatalf("ForceReconcileTime = %v, want %v", decision.ForceReconcileTime, in.Now)
	}
	if !decision.ConsumeForce {
		t.Fatal("a pass that acted on the request must take it away")
	}
	if !slices.Contains(decision.RemoveConditions, helmv1alpha1.ConditionTypeReconciling) {
		t.Fatal("the progress condition the forced pass raised must be removed again")
	}
}

func TestEvaluateMaintenanceReportsItselfAlone(t *testing.T) {
	in := baseInputs()
	in.Outcome = MaintenanceChanged{Result: internalobj.MaintenanceOutcome{Activated: true}}

	decision := Evaluate(in)

	managed := condition(t, decision, helmv1alpha1.ConditionTypeManaged)
	if managed.Status != metav1.ConditionFalse || managed.Reason != helmv1alpha1.ReasonMaintenanceModeActive {
		t.Fatalf("Managed = %s/%s", managed.Status, managed.Reason)
	}
	ready := condition(t, decision, helmv1alpha1.ConditionTypeReady)
	if ready.Reason != helmv1alpha1.ReasonMaintenanceModeActive {
		t.Fatalf("Ready reason = %q, want the maintenance verdict", ready.Reason)
	}
	if apimeta.FindStatusCondition(decision.Conditions, helmv1alpha1.ConditionTypeInstalled) != nil {
		t.Fatal("a maintenance pass must not speak about installing the chart")
	}
}

func TestEvaluateMaintenanceHoldLeavesTheObservedGenerationBehind(t *testing.T) {
	in := baseInputs()
	in.ObservedGeneration = 1
	in.Outcome = InMaintenance{}

	decision := Evaluate(in)

	if decision.ObservedGeneration != nil {
		t.Fatalf("ObservedGeneration = %d, want it left where it was", *decision.ObservedGeneration)
	}
	if !decision.ConsumeForce {
		t.Fatal("a request that can never be acted on must be dropped")
	}
	if !slices.Contains(decision.RemoveConditions, helmv1alpha1.ConditionTypeReconciling) {
		t.Fatal("Reconciling must not be left reporting work in flight forever")
	}
}

func TestEvaluateAdvancesTheObservedGeneration(t *testing.T) {
	in := baseInputs()
	in.ObservedGeneration = 1
	in.Outcome = ChartReached{
		Internal: ServedByHelmChart{Result: internalobj.ChartOutcome{Artifact: &meta.Artifact{Revision: "6.7.1"}, Internal: readyInternal()}},
	}

	decision := Evaluate(in)

	if decision.ObservedGeneration == nil || *decision.ObservedGeneration != in.Generation {
		t.Fatalf("ObservedGeneration = %v, want %d", decision.ObservedGeneration, in.Generation)
	}
}

func TestEvaluateCarriesTheObservedGeneration(t *testing.T) {
	tests := []struct {
		name    string
		inputs  func(*Inputs)
		advance bool
	}{
		{
			name: "a settled release finished with the spec",
			inputs: func(in *Inputs) {
				in.Outcome = ChartReached{
					Internal: ServedByHelmChart{Result: internalobj.ChartOutcome{
						Artifact: &meta.Artifact{Revision: "6.7.1"},
						Internal: readyInternal(),
					}},
				}
			},
			advance: true,
		},
		{
			name: "a step that will be retried has not",
			inputs: func(in *Inputs) {
				in.Outcome = StepFailed{Failure{
					Reason:  helmv1alpha1.ReasonRBACSetupFailed,
					Message: "Failed to set up the release identity",
					Err:     errors.New("forbidden"),
				}}
			},
			advance: false,
		},
		{
			name: "a step that gave up for good has",
			inputs: func(in *Inputs) {
				in.Outcome = StepFailed{Failure{
					Reason:   helmv1alpha1.ReasonForeignRBACObject,
					Message:  "Refusing to adopt an identity owned by someone else",
					Err:      errors.New("owned elsewhere"),
					Terminal: true,
				}}
			},
			advance: true,
		},
		{
			name: "an internal object still working on the spec has not",
			inputs: func(in *Inputs) {
				in.Outcome = ChartReached{
					Internal: ServedByHelmChart{Result: internalobj.ChartOutcome{
						Artifact: &meta.Artifact{Revision: "6.7.1"},
						Internal: internalobj.InternalObjectState{
							Observed: true,
							Status:   metav1.ConditionUnknown,
							Reason:   helmv1alpha1.ReasonReconciling,
							Message:  "Pulling the chart",
						},
					}},
				}
			},
			advance: false,
		},
		{
			name: "an internal object that gave up on the spec has",
			inputs: func(in *Inputs) {
				in.Outcome = ChartReached{
					Internal: ServedByHelmChart{Result: internalobj.ChartOutcome{
						Artifact: &meta.Artifact{Revision: "6.7.1"},
						Internal: internalobj.InternalObjectState{
							Observed: true,
							Status:   metav1.ConditionFalse,
							Reason:   helmv1alpha1.ReasonFailed,
							Message:  "No chart version matches the constraint",
							Stalled:  true,
						},
					}},
				}
			},
			advance: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			in := baseInputs()
			in.ObservedGeneration = 1
			test.inputs(&in)

			decision := Evaluate(in)

			switch {
			case test.advance && (decision.ObservedGeneration == nil || *decision.ObservedGeneration != in.Generation):
				t.Fatalf("ObservedGeneration = %v, want %d", decision.ObservedGeneration, in.Generation)
			case !test.advance && decision.ObservedGeneration != nil:
				t.Fatalf("ObservedGeneration = %d, want it left where it was", *decision.ObservedGeneration)
			}
		})
	}
}

func TestEvaluateReportsTheCauseOfEveryVerdict(t *testing.T) {
	cause := errors.New("boom")

	tests := []struct {
		name  string
		build func(Inputs) Inputs
	}{
		{
			name: "terminal failure",
			build: func(in Inputs) Inputs {
				in.Outcome = StepFailed{Failure{
					Reason: helmv1alpha1.ReasonFailed, Message: "bad spec", Err: cause, Terminal: true,
				}}

				return in
			},
		},
		{
			name: "step failure",
			build: func(in Inputs) Inputs {
				in.Outcome = StepFailed{Failure{Reason: helmv1alpha1.ReasonFailed, Message: "step failed", Err: cause}}

				return in
			},
		},
		{
			name: "maintenance write failure",
			build: func(in Inputs) Inputs {
				in.Outcome = MaintenanceChanged{Result: internalobj.MaintenanceOutcome{Err: cause}}

				return in
			},
		},
		{
			name: "chart write failure",
			build: func(in Inputs) Inputs {
				in.Outcome = ChartReached{
					Internal: ServedByHelmChart{Result: internalobj.ChartOutcome{Err: cause}},
				}

				return in
			},
		},
		{
			name: "chart source could not be resolved",
			build: func(in Inputs) Inputs {
				in.Outcome = ChartReached{
					Internal: SourceUnresolved{Failure: Failure{
						Reason:  helmv1alpha1.ReasonChartFetchFailed,
						Message: "Failed to resolve the desired chart version",
						Err:     cause,
					}},
				}

				return in
			},
		},
		{
			name: "release write failure",
			build: func(in Inputs) Inputs {
				in.Outcome = ChartReached{
					Internal: ServedByHelmChart{Result: internalobj.ChartOutcome{Artifact: &meta.Artifact{}, Internal: readyInternal()}},
					Release:  &internalobj.ReleaseOutcome{Err: cause},
				}

				return in
			},
		},
		{
			name: "artifact probe failure",
			build: func(in Inputs) Inputs {
				in.Outcome = ChartReached{
					Internal: ServedByOCIRepository{Result: internalobj.OCIRepoOutcome{
						ProbeErr:     cause,
						ProbeReason:  helmv1alpha1.ReasonOCIFetchFailed,
						ProbeMessage: "Failed to examine the chart artifact",
					}},
				}

				return in
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			decision := Evaluate(tt.build(baseInputs()))

			if decision.Reported == nil {
				t.Fatal("the cause behind the verdict was not carried out")
			}
			if !errors.Is(decision.Reported.Err, cause) {
				t.Fatalf("Reported.Err = %v, want %v", decision.Reported.Err, cause)
			}
			if decision.Reported.Reason == "" || decision.Reported.Message == "" {
				t.Fatalf("Reported = %+v, want it named", *decision.Reported)
			}
		})
	}
}

func TestEvaluateReportsNothingWhenNothingFailed(t *testing.T) {
	in := baseInputs()
	in.Outcome = ChartReached{
		Internal: ServedByHelmChart{Result: internalobj.ChartOutcome{Artifact: &meta.Artifact{}, Internal: readyInternal()}},
		Release:  &internalobj.ReleaseOutcome{Internal: readyInternal(), ChartDeployed: true},
	}

	if decision := Evaluate(in); decision.Reported != nil {
		t.Fatalf("Reported = %+v, want none on a healthy pass", *decision.Reported)
	}
}

func TestEvaluateRefusesAPassWithNoOutcome(t *testing.T) {
	decision := Evaluate(baseInputs())

	if decision.Err == nil {
		t.Fatal("a pass that ended in no outcome must be handed to the work queue")
	}
	if decision.Conditions != nil || decision.RemoveConditions != nil {
		t.Fatalf("decision = %+v, want no status written", decision)
	}
	if decision.ObservedGeneration != nil {
		t.Fatal("a pass that said nothing must not claim to have observed the spec")
	}
}

func TestEvaluateTransientStepFailureIsHandedToTheWorkQueue(t *testing.T) {
	cause := errors.New("etcdserver: request timed out")

	tests := []struct {
		name    string
		failure Failure
		retried bool
	}{
		{
			name: "a transient namespace failure",
			failure: Failure{
				Reason:  helmv1alpha1.ReasonFailed,
				Message: "Failed to reconcile target namespace",
				Err:     cause,
			},
			retried: true,
		},
		{
			name: "a transient identity failure",
			failure: Failure{
				Reason:  helmv1alpha1.ReasonRBACSetupFailed,
				Message: "Failed to set up the release identity",
				Err:     cause,
			},
			retried: true,
		},
		{
			name: "a foreign object under the derived name",
			failure: Failure{
				Reason:   helmv1alpha1.ReasonForeignRBACObject,
				Message:  "Refusing to adopt an identity owned by someone else",
				Err:      errors.New("owned elsewhere"),
				Terminal: true,
			},
			retried: false,
		},
		{
			name: "a repository that does not exist yet",
			failure: Failure{
				Reason:      helmv1alpha1.ReasonFailed,
				Message:     "Failed to get internal repository",
				Err:         cause,
				AwaitsWatch: true,
			},
			retried: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := baseInputs()
			in.Outcome = StepFailed{tt.failure}

			decision := Evaluate(in)

			if retried := decision.Err != nil; retried != tt.retried {
				t.Fatalf("Err = %v, want retried = %v", decision.Err, tt.retried)
			}
		})
	}
}

func TestEvaluateFailedMaintenanceSwitchLeavesManagedAlone(t *testing.T) {
	cause := errors.New("conflict")

	in := baseInputs()
	in.Outcome = MaintenanceChanged{Result: internalobj.MaintenanceOutcome{Activated: true, Err: cause}}

	decision := Evaluate(in)

	if apimeta.FindStatusCondition(decision.Conditions, helmv1alpha1.ConditionTypeManaged) != nil {
		t.Fatal("a failed switch must not write Managed: the suspend flag did not move")
	}

	ready := condition(t, decision, helmv1alpha1.ConditionTypeReady)
	if ready.Status != metav1.ConditionFalse || ready.Reason != helmv1alpha1.ReasonFailed {
		t.Fatalf("Ready = %s/%s, want False/%s", ready.Status, ready.Reason, helmv1alpha1.ReasonFailed)
	}

	progress := condition(t, decision, helmv1alpha1.ConditionTypeReconciling)
	if progress.Status != metav1.ConditionTrue {
		t.Fatalf("Reconciling = %s, want True while the switch is being retried", progress.Status)
	}
	if !errors.Is(decision.Err, cause) {
		t.Fatalf("Err = %v, want the cause handed to the work queue", decision.Err)
	}
	if decision.ObservedGeneration != nil {
		t.Fatal("a switch that will be retried has not finished with the spec")
	}
}

func TestEvaluateMaintenanceHoldIsNotReady(t *testing.T) {
	in := baseInputs()
	in.Outcome = InMaintenance{}

	decision := Evaluate(in)

	for _, conditionType := range []string{
		helmv1alpha1.ConditionTypeManaged,
		helmv1alpha1.ConditionTypeReady,
	} {
		cond := condition(t, decision, conditionType)
		if cond.Status != metav1.ConditionFalse || cond.Reason != helmv1alpha1.ReasonMaintenanceModeActive {
			t.Fatalf("%s = %s/%s, want False/%s",
				conditionType, cond.Status, cond.Reason, helmv1alpha1.ReasonMaintenanceModeActive)
		}
	}
}

func TestEvaluateLiftedMaintenanceReportsManagedAndKeepsGoing(t *testing.T) {
	in := baseInputs()
	in.MaintenanceLifted = true
	in.Outcome = ChartReached{
		Internal: ServedByHelmChart{Result: internalobj.ChartOutcome{
			Internal: internalobj.InternalObjectState{
				Status:  metav1.ConditionUnknown,
				Reason:  helmv1alpha1.ReasonReconciling,
				Message: "pulling chart",
			},
		}},
	}

	decision := Evaluate(in)

	managed := condition(t, decision, helmv1alpha1.ConditionTypeManaged)
	if managed.Status != metav1.ConditionTrue || managed.Reason != helmv1alpha1.ReasonMaintenanceModeInactive {
		t.Fatalf("Managed = %s/%s, want True/%s",
			managed.Status, managed.Reason, helmv1alpha1.ReasonMaintenanceModeInactive)
	}

	ready := condition(t, decision, helmv1alpha1.ConditionTypeReady)
	if ready.Status != metav1.ConditionUnknown || ready.Reason != helmv1alpha1.ReasonReconciling {
		t.Fatalf("Ready = %s/%s, want the verdict of the resumed work", ready.Status, ready.Reason)
	}
	if decision.ObservedGeneration != nil {
		t.Fatal("the resumed work has not finished with the spec yet")
	}
}

func TestEvaluateRefusesAPassThatEndedOnLiftedMaintenance(t *testing.T) {
	in := baseInputs()
	in.Outcome = MaintenanceChanged{Result: internalobj.MaintenanceOutcome{Activated: false}}

	decision := Evaluate(in)

	if decision.Err == nil {
		t.Fatal("a pass that ended on lifted maintenance must be handed to the work queue")
	}
	if decision.Conditions != nil {
		t.Fatalf("Conditions = %+v, want no status written", decision.Conditions)
	}
}

func TestEvaluateTransientInternalObjectFailureIsHandedToTheWorkQueue(t *testing.T) {
	cause := errors.New("etcdserver: request timed out")

	tests := []struct {
		name    string
		outcome ChartReached
		retried bool
	}{
		{
			name:    "the internal helm chart could not be written",
			outcome: ChartReached{Internal: ServedByHelmChart{Result: internalobj.ChartOutcome{Err: cause}}},
			retried: true,
		},
		{
			name:    "the internal oci repository could not be written",
			outcome: ChartReached{Internal: ServedByOCIRepository{Result: internalobj.OCIRepoOutcome{Err: cause}}},
			retried: true,
		},
		{
			name: "the internal helm release could not be written",
			outcome: ChartReached{
				Internal: ServedByHelmChart{Result: internalobj.ChartOutcome{
					Artifact: &meta.Artifact{Revision: "6.7.1"},
					Internal: readyInternal(),
				}},
				Release: &internalobj.ReleaseOutcome{Err: cause},
			},
			retried: true,
		},
		{
			name: "the superseded internal source could not be removed",
			outcome: ChartReached{Internal: SourceUnresolved{Failure: Failure{
				Reason:  helmv1alpha1.ReasonFailed,
				Message: "Repository change failed",
				Err:     cause,
			}}},
			retried: true,
		},
		{
			name: "an internal object that exists and is not ready yet",
			outcome: ChartReached{Internal: ServedByHelmChart{Result: internalobj.ChartOutcome{
				Internal: internalobj.InternalObjectState{
					Observed: true,
					Status:   metav1.ConditionFalse,
					Reason:   helmv1alpha1.ReasonChartFetchFailed,
					Message:  "failed to pull the chart",
				},
			}}},
			retried: false,
		},
		{
			name: "an artifact probe failure already on a timer",
			outcome: ChartReached{Internal: ServedByOCIRepository{Result: internalobj.OCIRepoOutcome{
				ProbeErr:          cause,
				ProbeReason:       helmv1alpha1.ReasonOCIFetchFailed,
				ProbeMessage:      "Failed to examine the chart artifact",
				ProbeRequeueAfter: time.Minute,
			}}},
			retried: false,
		},
		{
			name: "a terminal probe verdict about the artifact itself",
			outcome: ChartReached{Internal: ServedByOCIRepository{Result: internalobj.OCIRepoOutcome{
				ProbeErr:      cause,
				ProbeReason:   helmv1alpha1.ReasonUnsupportedChartArtifact,
				ProbeMessage:  "the tag does not address a packaged chart",
				ProbeTerminal: true,
			}}},
			retried: false,
		},
		{
			name: "a chart version the catalog does not have",
			outcome: ChartReached{Internal: SourceUnresolved{Failure: Failure{
				Reason:   helmv1alpha1.ReasonChartVersionNotFound,
				Message:  "Version 6.7.2 of chart podinfo is not in the catalog of repository example",
				Err:      errors.New("chart catalog does not have version"),
				BadInput: true,
			}}},
			retried: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := baseInputs()
			in.Outcome = tt.outcome

			decision := Evaluate(in)

			if retried := decision.Err != nil; retried != tt.retried {
				t.Fatalf("Err = %v, want retried = %v", decision.Err, tt.retried)
			}

			if cause := failureCause(tt.outcome); tt.retried && !errors.Is(decision.Err, cause) {
				t.Fatalf("Err = %v, want it to wrap %v", decision.Err, cause)
			}
		})
	}
}

func failureCause(outcome Outcome) error {
	switch o := outcome.(type) {
	case StepFailed:
		return o.Failure.Err
	case ChartReached:
		if o.Release != nil && o.Release.Err != nil {
			return o.Release.Err
		}

		switch src := o.Internal.(type) {
		case SourceUnresolved:
			return src.Failure.Err
		case ServedByHelmChart:
			return src.Result.Err
		case ServedByOCIRepository:
			return src.Result.Err
		}
	}

	return nil
}

func TestEvaluateFailedReleaseWriteBehindAFailingSourceIsHandedToTheWorkQueue(t *testing.T) {
	cause := errors.New("etcdserver: request timed out")

	in := baseInputs()
	in.Outcome = ChartReached{
		Internal: ServedByHelmChart{Result: internalobj.ChartOutcome{
			Artifact: &meta.Artifact{Revision: "6.7.1"},
			Internal: internalobj.InternalObjectState{
				Observed: true,
				Status:   metav1.ConditionFalse,
				Reason:   helmv1alpha1.ReasonChartFetchFailed,
				Message:  "failed to pull the chart",
			},
		}},
		Release: &internalobj.ReleaseOutcome{Err: cause},
	}

	decision := Evaluate(in)

	if decision.Err == nil && decision.RequeueAfter == 0 {
		t.Fatal("the pass scheduled nothing: neither the source's watch nor the untouched release brings it back")
	}
	if !errors.Is(decision.Err, cause) {
		t.Fatalf("Err = %v, want it to wrap %v", decision.Err, cause)
	}

	for _, conditionType := range in.ConditionTypes {
		cond := condition(t, decision, conditionType)
		if cond.Status != metav1.ConditionFalse || cond.Reason != helmv1alpha1.ReasonChartFetchFailed {
			t.Fatalf("%s = %s/%s, want the failing source's verdict", conditionType, cond.Status, cond.Reason)
		}
	}
	if decision.Reported != nil {
		t.Fatalf("Reported = %+v, want none: the reported verdict carries no error of its own", decision.Reported)
	}
}

func TestEvaluateFailedReleaseWriteIsQueuedWhateverSourceServesTheChart(t *testing.T) {
	cause := errors.New("etcdserver: request timed out")

	in := baseInputs()
	in.Outcome = ChartReached{
		Internal: ServedByOCIRepository{Result: internalobj.OCIRepoOutcome{
			Artifact: &meta.Artifact{Revision: "6.7.1@sha256:" + strings.Repeat("a", 64)},
			Internal: internalobj.InternalObjectState{
				Observed: true,
				Status:   metav1.ConditionFalse,
				Reason:   helmv1alpha1.ReasonChartFetchFailed,
				Message:  "failed to pull the chart",
			},
		}},
		Release: &internalobj.ReleaseOutcome{Err: cause},
	}

	decision := Evaluate(in)

	if !errors.Is(decision.Err, cause) {
		t.Fatalf("Err = %v, want it to wrap %v", decision.Err, cause)
	}
}

func TestEvaluateStalledSourceKeepsAFailedReleaseWriteOutOfTheQueue(t *testing.T) {
	in := baseInputs()
	in.Outcome = ChartReached{
		Internal: ServedByHelmChart{Result: internalobj.ChartOutcome{
			Artifact: &meta.Artifact{Revision: "6.7.1"},
			Internal: internalobj.InternalObjectState{
				Observed: true,
				Stalled:  true,
				Status:   metav1.ConditionFalse,
				Reason:   helmv1alpha1.ReasonChartFetchFailed,
				Message:  "no chart version found for podinfo-9.9.9",
			},
		}},
		Release: &internalobj.ReleaseOutcome{Err: errors.New("etcdserver: request timed out")},
	}

	decision := Evaluate(in)

	if decision.Err != nil || decision.RequeueAfter != 0 {
		t.Fatalf("Err = %v, RequeueAfter = %v, want nothing scheduled: a stalled source waits for a new spec",
			decision.Err, decision.RequeueAfter)
	}
}

func TestEvaluateMissingChartVersionIsNotWorkInProgress(t *testing.T) {
	const message = "Version 6.7.2 of chart podinfo is not in the catalog of repository example"

	in := baseInputs()
	in.Outcome = ChartReached{Internal: SourceUnresolved{Failure: Failure{
		Reason:   helmv1alpha1.ReasonChartVersionNotFound,
		Message:  message,
		Err:      errors.New(`chart catalog does not have version "6.7.2": chart version not found`),
		BadInput: true,
	}}}

	decision := Evaluate(in)

	ready := condition(t, decision, helmv1alpha1.ConditionTypeReady)
	if ready.Status != metav1.ConditionFalse || ready.Reason != helmv1alpha1.ReasonChartVersionNotFound {
		t.Fatalf("Ready = %s/%s, want False/%s",
			ready.Status, ready.Reason, helmv1alpha1.ReasonChartVersionNotFound)
	}
	if ready.Message != message {
		t.Fatalf("Ready message = %q, want %q", ready.Message, message)
	}

	if cond := apimeta.FindStatusCondition(decision.Conditions, helmv1alpha1.ConditionTypeReconciling); cond != nil {
		t.Fatalf("Reconciling = %+v, want none: a version that is not offered is not work in flight", cond)
	}
	if !slices.Contains(decision.RemoveConditions, helmv1alpha1.ConditionTypeReconciling) {
		t.Fatal("Reconciling must be taken away, otherwise an earlier one keeps promising a retry")
	}
	if !slices.Contains(decision.RemoveConditions, helmv1alpha1.ConditionTypeStalled) {
		t.Fatal("Stalled must stay off: the version may be published without any edit to the spec")
	}
}

func TestEvaluateDoesNotRecordAChartAppliedFromAKeptArtifact(t *testing.T) {
	keptArtifact := internalobj.InternalObjectState{
		Observed: true,
		Status:   metav1.ConditionFalse,
		Reason:   helmv1alpha1.ReasonChartFetchFailed,
		Message:  "no chart version found for podinfo-6.7.2",
	}

	in := baseInputs()
	in.Outcome = ChartReached{
		Internal:          ServedByHelmChart{Result: internalobj.ChartOutcome{Artifact: &meta.Artifact{Revision: "6.7.1"}, Internal: keptArtifact}},
		Release:           &internalobj.ReleaseOutcome{Internal: readyInternal(), ChartDeployed: true},
		ChartInfoOutdated: true,
	}

	decision := Evaluate(in)

	if decision.ApplyChart {
		t.Fatal("a chart that was never fetched must not be recorded as the applied one")
	}
}

func TestEvaluateRecordsValuesAppliedFromAKeptArtifact(t *testing.T) {
	keptArtifact := internalobj.InternalObjectState{
		Observed: true,
		Status:   metav1.ConditionFalse,
		Reason:   helmv1alpha1.ReasonChartFetchFailed,
		Message:  "no chart version found for podinfo-6.7.2",
	}

	in := baseInputs()
	in.Outcome = ChartReached{
		Internal: ServedByHelmChart{Result: internalobj.ChartOutcome{Artifact: &meta.Artifact{Revision: "6.7.1"}, Internal: keptArtifact}},
		Release: &internalobj.ReleaseOutcome{
			Internal:      readyInternal(),
			ChartDeployed: false,
			History:       helmv2.Snapshots{{Status: "deployed", ConfigDigest: "sha256:edited"}},
		},
		ChartInfoOutdated: true,
		ValuesDigest:      "sha256:edited",
	}

	decision := Evaluate(in)

	if !decision.ApplyValues {
		t.Fatal("values the deployed revision carries must be recorded whatever the source makes of the chart")
	}
	if decision.ApplyChart {
		t.Fatal("a chart that was never fetched must not be recorded as the applied one")
	}

	ready := condition(t, decision, helmv1alpha1.ConditionTypeReady)
	if ready.Status != metav1.ConditionFalse || ready.Reason != helmv1alpha1.ReasonChartFetchFailed {
		t.Fatalf("Ready = %s/%s, want the failed fetch still reported", ready.Status, ready.Reason)
	}
	if apimeta.FindStatusCondition(decision.Conditions, helmv1alpha1.ConditionTypeReconciling) == nil {
		t.Fatal("a release whose chart is not the one asked for is still work in flight")
	}
}

func TestEvaluateDoesNotRecordValuesOfTheReleaseBeingReplaced(t *testing.T) {
	in := baseInputs()
	in.Outcome = ChartReached{
		Internal: ServedByHelmChart{Result: internalobj.ChartOutcome{Artifact: &meta.Artifact{Revision: "6.7.2"}, Internal: readyInternal()}},
		Release: &internalobj.ReleaseOutcome{
			Internal:      readyInternal(),
			ChartDeployed: false,
			History:       helmv2.Snapshots{{Status: "deployed", ConfigDigest: "sha256:previous"}},
		},
		ChartInfoOutdated: true,
		ValuesDigest:      "sha256:edited",
	}

	decision := Evaluate(in)

	if decision.ApplyValues {
		t.Fatal("values the release being replaced was installed with must not be recorded as the applied ones")
	}
	if decision.ApplyChart {
		t.Fatal("the record of what was applied must not advance ahead of the rollout")
	}
}

func TestEvaluateDoesNotRecordValuesAReleaseThatFailedWasGiven(t *testing.T) {
	in := baseInputs()
	in.Outcome = ChartReached{
		Internal: ServedByHelmChart{Result: internalobj.ChartOutcome{Artifact: &meta.Artifact{Revision: "6.7.1"}, Internal: readyInternal()}},
		Release: &internalobj.ReleaseOutcome{
			Internal: internalobj.InternalObjectState{
				Observed: true,
				Status:   metav1.ConditionFalse,
				Reason:   helmv1alpha1.ReasonReleaseFailed,
				Message:  "upgrade retries exhausted",
			},
			ChartDeployed: true,
			History:       helmv2.Snapshots{{Status: "deployed", ConfigDigest: "sha256:previous"}},
		},
		ValuesDigest: "sha256:edited",
	}

	if Evaluate(in).ApplyValues {
		t.Fatal("values a failed release never got as far as deploying must not be recorded")
	}
}

func TestEvaluateRecordsValuesOnlyOnAReleaseThatReportedSuccess(t *testing.T) {
	const digest = "sha256:edited"

	for name, state := range map[string]internalobj.InternalObjectState{
		"failed on the spec it was given": {
			Observed: true,
			Status:   metav1.ConditionFalse,
			Reason:   helmv1alpha1.ReasonReleaseFailed,
			Message:  "upgrade retries exhausted",
		},
		"still applying the spec it was given": {
			Observed: true,
			Status:   metav1.ConditionUnknown,
			Reason:   helmv1alpha1.ReasonReconciling,
			Message:  "running upgrade",
		},
		"has not observed the spec it was given": {
			Status:  metav1.ConditionTrue,
			Reason:  "Succeeded",
			Message: "Helm upgrade succeeded",
		},
	} {
		t.Run(name, func(t *testing.T) {
			in := baseInputs()
			in.Outcome = ChartReached{
				Internal: ServedByHelmChart{Result: internalobj.ChartOutcome{Artifact: &meta.Artifact{Revision: "6.7.1"}, Internal: readyInternal()}},
				Release: &internalobj.ReleaseOutcome{
					Internal:      state,
					ChartDeployed: true,
					History:       helmv2.Snapshots{{Status: "deployed", ConfigDigest: digest}},
				},
				ValuesDigest: digest,
			}

			if Evaluate(in).ApplyValues {
				t.Fatal("values must be recorded only once the release itself reports success on the spec carrying them")
			}
		})
	}
}

func TestEvaluateDoesNotRecordValuesBehindAPendingSnapshot(t *testing.T) {
	const digest = "sha256:edited"

	in := baseInputs()
	in.Outcome = ChartReached{
		Internal: ServedByHelmChart{Result: internalobj.ChartOutcome{Artifact: &meta.Artifact{Revision: "6.7.1"}, Internal: readyInternal()}},
		Release: &internalobj.ReleaseOutcome{
			Internal:      readyInternal(),
			ChartDeployed: true,
			History: helmv2.Snapshots{
				{Version: 2, Status: "pending-upgrade", ConfigDigest: digest},
				{Version: 1, Status: "deployed", ConfigDigest: digest},
			},
		},
		ValuesDigest: digest,
	}

	if Evaluate(in).ApplyValues {
		t.Fatal("an attempt that has not finished must not be read as the deployed revision")
	}
}
