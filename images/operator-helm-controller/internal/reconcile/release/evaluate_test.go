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
	"testing"
	"time"

	helmv2 "github.com/fluxcd/helm-controller/api/v2"
	"github.com/fluxcd/pkg/apis/meta"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
	"github.com/deckhouse/operator-helm/internal/services"
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

func readyInternal() services.InternalObjectState {
	return services.InternalObjectState{
		Observed: true,
		Status:   metav1.ConditionTrue,
		Reason:   "Succeeded",
		Message:  "stored artifact",
	}
}

// TestEvaluateTerminalFailureStalls pins what tells a terminal failure apart from
// every other one: nothing this controller does resolves it, so it is reported as
// Stalled and the pass is not handed back to the work queue.
func TestEvaluateTerminalFailureStalls(t *testing.T) {
	in := baseInputs()
	in.Step = &Failure{
		Reason:   helmv1alpha1.ReasonFailed,
		Message:  "Target namespace cannot be a system namespace",
		Err:      errors.New("system namespace"),
		Terminal: true,
	}

	decision := Evaluate(in)

	ready := condition(t, decision, helmv1alpha1.ConditionTypeReady)
	if ready.Status != metav1.ConditionFalse || ready.Reason != helmv1alpha1.ReasonFailed {
		t.Fatalf("Ready = %s/%s, want False/%s", ready.Status, ready.Reason, helmv1alpha1.ReasonFailed)
	}

	stalled := condition(t, decision, helmv1alpha1.ConditionTypeStalled)
	if stalled.Status != metav1.ConditionTrue || stalled.Message != in.Step.Message {
		t.Fatalf("Stalled = %s/%q, want True carrying the cause", stalled.Status, stalled.Message)
	}
	if slices.Contains(decision.RemoveConditions, helmv1alpha1.ConditionTypeStalled) {
		t.Fatal("Stalled must not be both written and removed")
	}
	if decision.Err != nil {
		t.Fatalf("Err = %v, want none: retrying cannot change the cause", decision.Err)
	}
}

// TestEvaluateTerminalFailureIsNeverRetried pins that Terminal overrides a Retry the
// caller left set. The two contradict each other, and burning the work queue's rate
// limiter on an attempt whose outcome is already known is the worse reading.
func TestEvaluateTerminalFailureIsNeverRetried(t *testing.T) {
	in := baseInputs()
	in.Step = &Failure{
		Reason:   helmv1alpha1.ReasonForeignAccessObject,
		Message:  "role team-a/operator-helm-application already exists and is not managed by the operator",
		Err:      errors.New("foreign role"),
		Terminal: true,
		Retry:    true,
	}

	decision := Evaluate(in)

	if decision.Err != nil {
		t.Fatalf("Err = %v, want none", decision.Err)
	}
	condition(t, decision, helmv1alpha1.ConditionTypeStalled)
}

// TestEvaluateTerminalFailureDropsTheForceRequest pins why a terminal failure takes
// the force annotation away. It is matched on presence alone, so a request left
// behind makes the next one change nothing about the object and fire no event —
// which is exactly how whoever corrected the cause asks to be retried. The stamp is
// not written: it means the request was acted on, and this one was dropped.
func TestEvaluateTerminalFailureDropsTheForceRequest(t *testing.T) {
	in := baseInputs()
	in.Step = &Failure{
		Reason:   helmv1alpha1.ReasonForeignAccessObject,
		Message:  "role binding team-a/app already exists and is not managed by the operator",
		Err:      errors.New("foreign binding"),
		Terminal: true,
	}

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

// TestEvaluateRecoverableFailureKeepsTheForceRequest is the counterpart: a failure
// that can still come good leaves the request for the pass that can honour it.
func TestEvaluateRecoverableFailureKeepsTheForceRequest(t *testing.T) {
	in := baseInputs()
	in.Step = &Failure{
		Reason:  helmv1alpha1.ReasonAccessSetupFailed,
		Message: "Failed to set up the release identity",
		Err:     errors.New("forbidden"),
		Retry:   true,
	}

	if decision := Evaluate(in); decision.ConsumeForce {
		t.Fatal("a recoverable failure must leave the force request for the pass that can honour it")
	}
}

// TestEvaluateTerminalProbeVerdictStalls pins the one terminal verdict that does not
// arrive as a Failure: the registry rejected the pull, or what it serves is not a
// chart. The probe already schedules nothing for it, and Stalled is what says so to
// a reader — without it the release looks like one still on its way up.
func TestEvaluateTerminalProbeVerdictStalls(t *testing.T) {
	in := baseInputs()
	in.OCIRepo = &services.OCIRepoOutcome{
		ProbeErr:      errors.New("unauthorized"),
		ProbeReason:   helmv1alpha1.ReasonAuthenticationFailed,
		ProbeMessage:  "repository oci://example.com/charts rejected the credentials (HTTP 401)",
		ProbeTerminal: true,
	}

	decision := Evaluate(in)

	ready := condition(t, decision, helmv1alpha1.ConditionTypeReady)
	if ready.Status != metav1.ConditionFalse || ready.Reason != helmv1alpha1.ReasonAuthenticationFailed {
		t.Fatalf("Ready = %s/%s, want False/%s", ready.Status, ready.Reason, helmv1alpha1.ReasonAuthenticationFailed)
	}

	stalled := condition(t, decision, helmv1alpha1.ConditionTypeStalled)
	if stalled.Status != metav1.ConditionTrue || stalled.Message != in.OCIRepo.ProbeMessage {
		t.Fatalf("Stalled = %s/%q, want True carrying the probe verdict", stalled.Status, stalled.Message)
	}
	if slices.Contains(decision.RemoveConditions, helmv1alpha1.ConditionTypeStalled) {
		t.Fatal("Stalled must not be both written and removed")
	}
}

// TestEvaluateRecoverableProbeFailureDoesNotStall is the counterpart: a probe that
// failed for a reason that may pass on its own leaves Stalled off and rides the
// timer the probe asked for.
func TestEvaluateRecoverableProbeFailureDoesNotStall(t *testing.T) {
	in := baseInputs()
	in.OCIRepo = &services.OCIRepoOutcome{
		ProbeErr:          errors.New("connection refused"),
		ProbeReason:       helmv1alpha1.ReasonOCIFetchFailed,
		ProbeMessage:      "Failed to examine the chart artifact: connection refused",
		ProbeRequeueAfter: time.Minute,
	}

	decision := Evaluate(in)

	if !slices.Contains(decision.RemoveConditions, helmv1alpha1.ConditionTypeStalled) {
		t.Fatal("a recoverable probe failure must take Stalled away")
	}
	if decision.RequeueAfter != time.Minute {
		t.Fatalf("RequeueAfter = %v, want the probe's own timer", decision.RequeueAfter)
	}
}

// TestEvaluateStepFailureOwnsReadyAlone pins the reach of a step failure: the pass
// never got as far as the chart, so of the projected conditions it may say the
// release is not ready and nothing more. It is not stalled — a watch on what the
// step needed, or the work queue, wakes the release — and saying so is what the
// progress condition beside Ready is for.
func TestEvaluateStepFailureOwnsReadyAlone(t *testing.T) {
	in := baseInputs()
	in.Step = &Failure{
		Reason:  helmv1alpha1.ReasonAccessSetupFailed,
		Message: "Failed to set up the release identity",
		Err:     errors.New("forbidden"),
		Retry:   true,
	}

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
	if !errors.Is(decision.Err, in.Step.Err) {
		t.Fatalf("Err = %v, want the step's error handed to the work queue", decision.Err)
	}
}

// TestEvaluateReportsWorkInFlight pins the progress condition on the ordinary path:
// an internal object that has not yet reported on the spec it was given is work in
// flight, and the release says so with that object's own words rather than a fixed
// message of its own.
func TestEvaluateReportsWorkInFlight(t *testing.T) {
	in := baseInputs()
	in.Chart = &services.ChartOutcome{
		Artifact: &meta.Artifact{Revision: "6.7.1"},
		Internal: services.InternalObjectState{
			Status:  metav1.ConditionUnknown,
			Reason:  helmv1alpha1.ReasonReconciling,
			Message: "pulling the chart",
		},
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

// TestEvaluateReportsARetryAfterAFailedRollout pins the other half: a failure that
// is not terminal has something coming back to it, so the release keeps reporting
// progress beside the failure itself.
func TestEvaluateReportsARetryAfterAFailedRollout(t *testing.T) {
	in := baseInputs()
	in.Chart = &services.ChartOutcome{Artifact: &meta.Artifact{Revision: "6.7.1"}, Internal: readyInternal()}
	in.Release = &services.ReleaseOutcome{
		ChartDeployed: true,
		Internal: services.InternalObjectState{
			Observed: true,
			Status:   metav1.ConditionFalse,
			Reason:   helmv1alpha1.ReasonReleaseFailed,
			Message:  "upgrade failed",
		},
	}

	decision := Evaluate(in)

	progress := condition(t, decision, helmv1alpha1.ConditionTypeReconciling)
	if progress.Status != metav1.ConditionTrue || progress.Reason != helmv1alpha1.ReasonProgressingWithRetry {
		t.Fatalf("Reconciling = %s/%s, want True/%s",
			progress.Status, progress.Reason, helmv1alpha1.ReasonProgressingWithRetry)
	}
}

// TestEvaluateSettledReleaseReportsNoProgress pins the removal: nothing is in
// flight, so the abnormal-true condition has to go, or kstatus reads a healthy
// release as one that never finishes.
func TestEvaluateSettledReleaseReportsNoProgress(t *testing.T) {
	in := baseInputs()
	in.Chart = &services.ChartOutcome{Artifact: &meta.Artifact{Revision: "6.7.1"}, Internal: readyInternal()}
	in.Release = &services.ReleaseOutcome{ChartDeployed: true, Internal: readyInternal()}

	decision := Evaluate(in)

	if apimeta.FindStatusCondition(decision.Conditions, helmv1alpha1.ConditionTypeReconciling) != nil {
		t.Fatal("a settled release must not report work in flight")
	}
	if !slices.Contains(decision.RemoveConditions, helmv1alpha1.ConditionTypeReconciling) {
		t.Fatal("Reconciling must be taken away once the release settled")
	}
}

// TestEvaluateStalledReleaseReportsNoProgress pins that Stalled outranks the
// progress condition, as it does in the repository package: a release that cannot
// proceed is not making progress, and reporting both would say two things at once.
func TestEvaluateStalledReleaseReportsNoProgress(t *testing.T) {
	in := baseInputs()
	in.OCIRepo = &services.OCIRepoOutcome{
		ProbeErr:      errors.New("unauthorized"),
		ProbeReason:   helmv1alpha1.ReasonAuthenticationFailed,
		ProbeMessage:  "repository oci://example.com/charts rejected the credentials (HTTP 401)",
		ProbeTerminal: true,
	}

	decision := Evaluate(in)

	condition(t, decision, helmv1alpha1.ConditionTypeStalled)
	if apimeta.FindStatusCondition(decision.Conditions, helmv1alpha1.ConditionTypeReconciling) != nil {
		t.Fatal("a stalled release must not also report work in flight")
	}
	if !slices.Contains(decision.RemoveConditions, helmv1alpha1.ConditionTypeReconciling) {
		t.Fatal("Reconciling must be taken away behind a terminal verdict")
	}
}

func TestEvaluateStepFailureWithoutRetryIsNotReturned(t *testing.T) {
	in := baseInputs()
	in.Step = &Failure{
		Reason:       helmv1alpha1.ReasonChartClaimConflict,
		Message:      "chart is already used",
		RequeueAfter: chartClaimConflictRequeueInterval,
	}

	decision := Evaluate(in)

	if decision.Err != nil {
		t.Fatalf("Err = %v, want none: the recovery rides on the timer", decision.Err)
	}
	if decision.RequeueAfter != chartClaimConflictRequeueInterval {
		t.Fatalf("RequeueAfter = %v, want %v", decision.RequeueAfter, chartClaimConflictRequeueInterval)
	}
}

// TestEvaluateProjectsTheFirstFailingStep pins the rule a reader of the status
// applies: the verdict is the first step that did not succeed, and it reaches every
// condition type the release currently reports on.
func TestEvaluateProjectsTheFirstFailingStep(t *testing.T) {
	in := baseInputs()
	in.Chart = &services.ChartOutcome{Artifact: &meta.Artifact{Revision: "6.7.1"}, Internal: readyInternal()}
	in.Release = &services.ReleaseOutcome{
		Internal: services.InternalObjectState{
			Observed: true,
			Status:   metav1.ConditionFalse,
			Reason:   helmv1alpha1.ReasonReleaseFailed,
			Message:  "install retries exhausted",
		},
		ChartDeployed: true,
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

// TestEvaluateHoldsAReleaseThatHasNotRolledOut pins why the HelmRelease's own Ready
// is not enough: a chart-version change moves the artifact without touching its
// spec, so it can go on reporting the previous revision ready.
func TestEvaluateHoldsAReleaseThatHasNotRolledOut(t *testing.T) {
	in := baseInputs()
	in.Chart = &services.ChartOutcome{Artifact: &meta.Artifact{Revision: "6.7.2"}, Internal: readyInternal()}
	in.Release = &services.ReleaseOutcome{Internal: readyInternal(), ChartDeployed: false}
	in.ChartInfoOutdated = true

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
	in.Chart = &services.ChartOutcome{Artifact: &meta.Artifact{Revision: "6.7.1"}, Internal: readyInternal()}
	in.Release = &services.ReleaseOutcome{
		Internal:      readyInternal(),
		ChartDeployed: true,
		History:       helmv2.Snapshots{{Status: "deployed", ConfigDigest: "sha256:values"}},
	}
	in.ChartInfoOutdated = true
	in.ValuesDigest = "sha256:values"

	decision := Evaluate(in)

	if !decision.ApplyChart {
		t.Fatal("a rolled-out chart must be recorded as applied")
	}
	if !decision.ApplyValues {
		t.Fatal("values the deployed revision was installed with must be recorded as applied")
	}

	in.ValuesDigest = "sha256:other"
	if Evaluate(in).ApplyValues {
		t.Fatal("values the deployed revision was not installed with must not be recorded")
	}
}

// TestEvaluateNamesARemovedVersion pins that the cause is named only while it
// explains something: the marker outlives the tag's absence, so a child that is
// healthy again must not be relabelled with it.
func TestEvaluateNamesARemovedVersion(t *testing.T) {
	in := baseInputs()
	in.OCIRepo = &services.OCIRepoOutcome{
		Internal: services.InternalObjectState{
			Observed: true,
			Status:   metav1.ConditionFalse,
			Reason:   "ArtifactPullFailed",
			Message:  "not found",
		},
		VersionRemoved: true,
		Version:        "6.7.1",
		RepositoryName: "stable",
	}

	cond := condition(t, Evaluate(in), helmv1alpha1.ConditionTypeReady)
	if cond.Reason != helmv1alpha1.ReasonChartVersionRemoved {
		t.Fatalf("reason = %q, want %q", cond.Reason, helmv1alpha1.ReasonChartVersionRemoved)
	}

	in.OCIRepo.Internal = readyInternal()
	cond = condition(t, Evaluate(in), helmv1alpha1.ConditionTypeReady)
	if cond.Reason != "Succeeded" {
		t.Fatalf("reason = %q, want the healthy child's own reason untouched", cond.Reason)
	}
}

func TestEvaluateForcedPassStampsAndConsumesTheRequest(t *testing.T) {
	in := baseInputs()
	in.Forced = true
	in.Chart = &services.ChartOutcome{Artifact: &meta.Artifact{Revision: "6.7.1"}, Internal: readyInternal()}

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

// TestEvaluateMaintenanceReportsItselfAlone pins that a pass which only moved the
// release into maintenance says so on Managed and Ready and nothing else: it never
// touched the chart, so it has nothing to report about installing it.
func TestEvaluateMaintenanceReportsItselfAlone(t *testing.T) {
	in := baseInputs()
	in.Maintenance = &services.MaintenanceOutcome{Activated: true}

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

// TestEvaluateMaintenanceHoldLeavesTheObservedGenerationBehind pins the one pass
// that reports nothing about the spec it was given: a release already in maintenance
// stays there whatever its spec now says, and claiming the change was observed is
// what would make kstatus call it settled.
func TestEvaluateMaintenanceHoldLeavesTheObservedGenerationBehind(t *testing.T) {
	in := baseInputs()
	in.ObservedGeneration = 1
	in.DiscardForce = true

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
	in.Chart = &services.ChartOutcome{Artifact: &meta.Artifact{Revision: "6.7.1"}, Internal: readyInternal()}

	decision := Evaluate(in)

	if decision.ObservedGeneration == nil || *decision.ObservedGeneration != in.Generation {
		t.Fatalf("ObservedGeneration = %v, want %d", decision.ObservedGeneration, in.Generation)
	}
}

// TestEvaluateReportsTheCauseOfEveryVerdict pins that whichever kind of failure
// produced the verdict, the pass carries the cause out to be logged. Most release
// failures never reach the work queue, and several report a fixed message, so a
// verdict whose cause is not carried out is a cause nobody can read.
func TestEvaluateReportsTheCauseOfEveryVerdict(t *testing.T) {
	cause := errors.New("boom")

	tests := []struct {
		name  string
		build func(Inputs) Inputs
	}{
		{
			name: "terminal failure",
			build: func(in Inputs) Inputs {
				in.Step = &Failure{
					Reason: helmv1alpha1.ReasonFailed, Message: "bad spec", Err: cause, Terminal: true,
				}

				return in
			},
		},
		{
			name: "step failure",
			build: func(in Inputs) Inputs {
				in.Step = &Failure{Reason: helmv1alpha1.ReasonFailed, Message: "step failed", Err: cause}

				return in
			},
		},
		{
			name: "maintenance write failure",
			build: func(in Inputs) Inputs {
				in.Maintenance = &services.MaintenanceOutcome{Err: cause}

				return in
			},
		},
		{
			name: "chart write failure",
			build: func(in Inputs) Inputs {
				in.Chart = &services.ChartOutcome{Err: cause}

				return in
			},
		},
		{
			name: "chart source could not be resolved",
			build: func(in Inputs) Inputs {
				in.ChartSource = &Failure{
					Reason:  helmv1alpha1.ReasonChartFetchFailed,
					Message: "Failed to resolve the desired chart version",
					Err:     cause,
				}

				return in
			},
		},
		{
			name: "release write failure",
			build: func(in Inputs) Inputs {
				in.Chart = &services.ChartOutcome{Artifact: &meta.Artifact{}, Internal: readyInternal()}
				in.Release = &services.ReleaseOutcome{Err: cause}

				return in
			},
		},
		{
			name: "artifact probe failure",
			build: func(in Inputs) Inputs {
				in.OCIRepo = &services.OCIRepoOutcome{
					ProbeErr:     cause,
					ProbeReason:  helmv1alpha1.ReasonOCIFetchFailed,
					ProbeMessage: "Failed to examine the chart artifact",
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
	in.Chart = &services.ChartOutcome{Artifact: &meta.Artifact{}, Internal: readyInternal()}
	in.Release = &services.ReleaseOutcome{Internal: readyInternal(), ChartDeployed: true}

	if decision := Evaluate(in); decision.Reported != nil {
		t.Fatalf("Reported = %+v, want none on a healthy pass", *decision.Reported)
	}
}
