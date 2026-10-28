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
	"fmt"
	"slices"
	"time"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
	"github.com/deckhouse/operator-helm/internal/internalobj"
)

// Failure is one way a pass can fail, named the way the release reports it. Reason and
// Message are what the condition carries; Err is logged and may reach the work queue.
type Failure struct {
	Reason  string
	Message string
	Err     error

	// Terminal marks a failure nothing this controller can do will resolve. It is reported as
	// Stalled and excludes a retry.
	Terminal bool
	// AwaitsWatch marks a failure something else already brings the release back from, and
	// keeps Err away from the work queue.
	AwaitsWatch bool
	// BadInput marks a failure of the spec rather than of the work towards it. It is neither
	// Terminal nor progress: it is reported without Reconciling and without a retry.
	BadInput bool
	// RequeueAfter schedules another pass on a timer, for a cause no watch covers.
	RequeueAfter time.Duration
}

// Inputs carries everything Evaluate needs. It holds no clients and no clock: Now, the
// conditions and the outcome of the pass are supplied by the caller.
type Inputs struct {
	Generation         int64
	ObservedGeneration int64
	Now                time.Time

	// ConditionTypes is the set the pass's verdict is projected onto. It is derived
	// from the release's own status, which is why the caller computes it.
	ConditionTypes []string

	// Outcome is how the pass ended. Exactly one of the four is what happened.
	Outcome Outcome

	// MaintenanceLifted reports that this pass took the release out of maintenance mode and
	// went on with the work maintenance was holding back. It is not an outcome.
	MaintenanceLifted bool
}

// Outcome is how one reconcile pass ended. The interface is sealed: the four implementations
// below are the four endings a pass has.
type Outcome interface{ outcome() }

// StepFailed is a failure of one of the steps that run before the chart is resolved. It owns
// Ready alone, and adds Stalled when the Failure is terminal.
type StepFailed struct{ Failure Failure }

// MaintenanceChanged is the pass that moved the release into maintenance mode, or failed to
// move it either way. Such a pass reports nothing else: the chart is not touched.
type MaintenanceChanged struct {
	Result internalobj.MaintenanceOutcome
}

// InMaintenance is the pass that found the release already in maintenance and left it there.
// Nothing ran, and the release's spec may have changed since.
type InMaintenance struct{}

// ChartReached is the pass that got as far as the chart. Release is nil when the pass never
// reached the internal HelmRelease.
type ChartReached struct {
	// Forced reports whether this pass was requested through the force reconcile annotation.
	// It belongs to this ending alone.
	Forced bool

	Internal InternalSource
	Release  *internalobj.ReleaseOutcome
	// Inventory is what the internal release was read to hold when this pass did not write
	// it. It is nil when the pass wrote the release, which reports the list itself.
	Inventory *[]helmv1alpha1.InventoryEntry

	// ChartInfoOutdated and ValuesDigest are the two facts the record of what was last applied
	// turns on. ValuesDigest is compared against the deployed revision.
	ChartInfoOutdated bool
	ValuesDigest      string
}

// InternalSource is which internal source serves the release, or the failure to decide. The
// interface is sealed: a release is served by a HelmChart or an OCIRepository, never both.
type InternalSource interface{ internalSource() }

// SourceUnresolved stands in for the source outcome when the pass could not decide which
// internal source the release needs. It is projected across ConditionTypes like an outcome.
type SourceUnresolved struct{ Failure Failure }

type ServedByHelmChart struct{ Result internalobj.ChartOutcome }

type ServedByOCIRepository struct{ Result internalobj.OCIRepoOutcome }

func (StepFailed) outcome()         {}
func (MaintenanceChanged) outcome() {}
func (InMaintenance) outcome()      {}
func (ChartReached) outcome()       {}

func (SourceUnresolved) internalSource()      {}
func (ServedByHelmChart) internalSource()     {}
func (ServedByOCIRepository) internalSource() {}

// Decision is the full desired status plus the scheduling verdict. Removing a condition is
// expressed by its presence in RemoveConditions; the field writes are asked for, not performed.
type Decision struct {
	Conditions       []metav1.Condition
	RemoveConditions []string

	// ObservedGeneration is nil on a pass that has not finished with the spec it was given,
	// which is every pass that left something to come back to.
	ObservedGeneration *int64

	// ForceReconcileTime records that a force request was acted on, not that it succeeded.
	ForceReconcileTime *metav1.Time
	// Reported is the failure behind the verdict this pass wrote, when there is one. It is
	// carried out rather than logged in place, as most release failures never reach the queue.
	Reported *Failure
	// ConsumeForce asks for the force annotation to be removed. Only a pass that acted on the
	// request, or one that deliberately discarded it, may take it away.
	ConsumeForce bool
	// Inventory is the list of deployed objects this pass observed. It is nil on a pass that
	// never reached the internal HelmRelease, which leaves the recorded list standing.
	Inventory *[]helmv1alpha1.InventoryEntry
	// ApplyChart and ApplyValues ask for the record of what was last applied to be advanced to
	// what the spec asks for.
	ApplyChart  bool
	ApplyValues bool

	RequeueAfter time.Duration
	Err          error
}

// conditionState is one verdict: the value a condition would carry if this were the thing the
// release reports. It is what Evaluate compares and projects.
type conditionState struct {
	internalobj.InternalObjectState

	Failure Failure
}

// Evaluate derives the desired release status from the results of a single
// reconcile pass.
func Evaluate(in Inputs) Decision {
	decision := evaluate(in)

	if in.MaintenanceLifted {
		// Managed is written whatever the resumed work ends in; Ready is left to that work.
		decision.set(in, helmv1alpha1.ConditionTypeManaged, maintenanceLiftedState())
	}

	decision.ObservedGeneration = observedGeneration(in, decision)

	return decision
}

// observedGeneration decides whether this pass may claim to have finished with the spec it
// was given. Only a verdict the release will keep counts: settled, given up on for good, or
// refused as a fault of the spec.
func observedGeneration(in Inputs, decision Decision) *int64 {
	settled := apimeta.IsStatusConditionTrue(decision.Conditions, helmv1alpha1.ConditionTypeReady) ||
		apimeta.IsStatusConditionTrue(decision.Conditions, helmv1alpha1.ConditionTypeStalled) ||
		(decision.Reported != nil && decision.Reported.BadInput)
	if !settled {
		return nil
	}

	observed := max(in.Generation, in.ObservedGeneration)

	return &observed
}

func evaluate(in Inputs) Decision {
	decision := Decision{
		// kstatus abnormal-true conditions are present only while they apply, so both come off.
		RemoveConditions: []string{
			helmv1alpha1.ConditionTypeStalled,
			helmv1alpha1.ConditionTypeReconciling,
		},
	}

	switch outcome := in.Outcome.(type) {
	case StepFailed:
		return evaluateStepFailed(in, decision, outcome)
	case MaintenanceChanged:
		return evaluateMaintenanceChanged(in, decision, outcome)
	case InMaintenance:
		// Dropped, not processed: replaying an old request when maintenance is lifted would surprise.
		decision.ConsumeForce = true
		decision.set(in, helmv1alpha1.ConditionTypeManaged, maintenanceActiveState())
		decision.set(in, helmv1alpha1.ConditionTypeReady, maintenanceActiveState())

		return decision
	case ChartReached:
		return evaluateChartReached(in, decision, outcome)
	default:
		// Unreachable by construction; an empty decision would leave the old status standing forever.
		return Decision{Err: fmt.Errorf("reconcile pass ended in no outcome: %T", in.Outcome)}
	}
}

func evaluateStepFailed(in Inputs, decision Decision, outcome StepFailed) Decision {
	decision.set(in, helmv1alpha1.ConditionTypeReady, failureState(outcome.Failure))

	switch {
	case outcome.Failure.Terminal:
		decision.setAbnormal(in, helmv1alpha1.ConditionTypeStalled, outcome.Failure)
		// Dropped: the annotation is matched on presence, so leaving it would fire no later event.
		decision.ConsumeForce = true
	case outcome.Failure.AwaitsWatch:
		// Saying "retrying" here would promise an attempt nobody made.
		decision.setAbnormal(in, helmv1alpha1.ConditionTypeReconciling, Failure{
			Reason:  helmv1alpha1.ReasonReconciling,
			Message: "Waiting for what the failed step needs",
		})
	default:
		decision.setAbnormal(in, helmv1alpha1.ConditionTypeReconciling, Failure{
			Reason:  helmv1alpha1.ReasonProgressingWithRetry,
			Message: "Retrying the step that failed",
		})
	}

	decision.Reported = &outcome.Failure
	decision.Err = retryErr(outcome.Failure)
	decision.RequeueAfter = outcome.Failure.RequeueAfter

	return decision
}

func evaluateMaintenanceChanged(in Inputs, decision Decision, outcome MaintenanceChanged) Decision {
	if outcome.Result.Err != nil {
		// Managed is left where it was: the attempt did not change whether the release is suspended.
		return evaluateStepFailed(in, decision, StepFailed{Failure{
			Reason:  helmv1alpha1.ReasonFailed,
			Message: "Failed to change maintenance mode",
			Err:     outcome.Result.Err,
		}})
	}

	if !outcome.Result.Activated {
		// Unreachable: a pass that lifted maintenance resumes the work instead of ending here.
		return Decision{Err: errors.New("reconcile pass ended on lifted maintenance")}
	}

	decision.set(in, helmv1alpha1.ConditionTypeManaged, maintenanceActiveState())
	decision.set(in, helmv1alpha1.ConditionTypeReady, maintenanceActiveState())
	// Settling into maintenance drops an in-flight force request: it will not be acted on.
	decision.ConsumeForce = true

	return decision
}

func evaluateChartReached(in Inputs, decision Decision, outcome ChartReached) Decision {
	chart, release := evaluateChart(outcome.Internal), evaluateRelease(outcome.Release)

	var stalled bool

	verdict, found := decide(chart, release)
	if found {
		for _, conditionType := range in.ConditionTypes {
			state := lifecycleState(conditionType, verdict)
			if conditionType == helmv1alpha1.ConditionTypeReady && state.Status == metav1.ConditionTrue {
				state.Reason = helmv1alpha1.ReasonSuccess
			}

			decision.set(in, conditionType, state)
		}

		decision.Reported = reported(verdict)
	}

	oci, servedByOCI := outcome.Internal.(ServedByOCIRepository)

	switch {
	case servedByOCI && oci.Result.ProbeTerminal:
		// The verdict is about the artifact, so only a republish or a repository fix changes it.
		stalled = true

		decision.setAbnormal(in, helmv1alpha1.ConditionTypeStalled, Failure{
			Reason:  oci.Result.ProbeReason,
			Message: oci.Result.ProbeMessage,
		})
	case found && verdict.Stalled:
		// The internal object gave up on the spec it was given and only a new spec revives it.
		stalled = true

		decision.setAbnormal(in, helmv1alpha1.ConditionTypeStalled, Failure{
			Reason:  verdict.Reason,
			Message: verdict.Message,
		})
	}

	if progress, ok := evaluateReconciling(verdict, found, stalled); ok {
		decision.setAbnormal(in, helmv1alpha1.ConditionTypeReconciling, progress)
	}

	switch {
	case outcome.Release != nil && outcome.Release.Err == nil:
		decision.Inventory = &outcome.Release.Inventory
	case outcome.Inventory != nil:
		decision.Inventory = outcome.Inventory
	}

	if outcome.Forced {
		decision.ForceReconcileTime = &metav1.Time{Time: in.Now}
		decision.ConsumeForce = true
	}

	// Recorded only against an artifact of the spec asking for it, never a kept older one.
	_, current, resolved := artifactRevision(outcome.Internal)
	decision.ApplyChart = resolved && current && release.Ready() && outcome.ChartInfoOutdated
	// Values rest on different evidence: helm-controller applies a values edit from a kept artifact.
	decision.ApplyValues = releaseReported(outcome.Release) && valuesDeployed(outcome)
	decision.RequeueAfter = probeRequeueAfter(outcome.Internal)
	if decision.RequeueAfter == 0 {
		decision.RequeueAfter = verdict.Failure.RequeueAfter
	}

	if !stalled && decision.RequeueAfter == 0 {
		// Every state the pass produced is offered, not only the one the status reports.
		decision.Err = retryErr(chart.Failure, release.Failure)
	}

	return decision
}

// evaluateReconciling decides the kstatus progress condition: raised while the pass has left
// something to wait for, absent otherwise. A stall or a bad input outranks it.
func evaluateReconciling(verdict conditionState, found, stalled bool) (Failure, bool) {
	if stalled || !found || verdict.Failure.BadInput {
		return Failure{}, false
	}

	switch verdict.Status {
	case metav1.ConditionUnknown:
		return Failure{Reason: verdict.Reason, Message: verdict.Message}, true
	case metav1.ConditionFalse:
		if verdict.Failure.AwaitsWatch {
			// Nothing failed here, so the message names the wait rather than an attempt nobody made.
			return Failure{
				Reason:  helmv1alpha1.ReasonReconciling,
				Message: "Waiting for the repository to resolve the chart version",
			}, true
		}

		return Failure{
			Reason:  helmv1alpha1.ReasonProgressingWithRetry,
			Message: "Retrying after a failed reconcile",
		}, true
	default:
		return Failure{}, false
	}
}

// decide picks the one verdict the release reports: the first step that did not succeed, or
// the last one when every step succeeded. An empty verdict is not an answer.
func decide(states ...conditionState) (conditionState, bool) {
	var verdict conditionState
	var found bool

	for _, state := range states {
		if state.Status == "" || state.Reason == "" {
			continue
		}

		verdict, found = state, true
		if !state.Ready() {
			break
		}
	}

	return verdict, found
}

// evaluateChart reduces whichever internal source this release needs to one verdict.
func evaluateChart(src InternalSource) conditionState {
	switch source := src.(type) {
	case SourceUnresolved:
		return failureState(source.Failure)
	case ServedByHelmChart:
		if source.Result.Err != nil {
			return failureState(Failure{
				Reason:  helmv1alpha1.ReasonHelmChartFailed,
				Message: "Failed to create helm chart",
				Err:     source.Result.Err,
			})
		}

		return conditionState{InternalObjectState: source.Result.Internal}
	case ServedByOCIRepository:
		return evaluateOCIRepo(source.Result)
	default:
		return conditionState{}
	}
}

func evaluateOCIRepo(out internalobj.OCIRepoOutcome) conditionState {
	if out.ProbeErr != nil {
		return failureState(Failure{Reason: out.ProbeReason, Message: out.ProbeMessage, Err: out.ProbeErr})
	}

	if out.Err != nil {
		return failureState(Failure{
			Reason:  helmv1alpha1.ReasonFailed,
			Message: "Failed to reconcile oci repository",
			Err:     out.Err,
		})
	}

	state := conditionState{InternalObjectState: out.Internal}

	if out.VersionRemoved && state.Status != metav1.ConditionTrue {
		// Named here, or the user sees only the source controller's opaque "not found".
		state.Reason = helmv1alpha1.ReasonChartVersionRemoved
		state.Message = "Version " + out.Version + " is no longer offered by repository " +
			out.RepositoryName + ": " + state.Message
	}

	return state
}

func evaluateRelease(out *internalobj.ReleaseOutcome) conditionState {
	if out == nil {
		return conditionState{}
	}

	if out.Err != nil {
		return failureState(Failure{
			Reason:  helmv1alpha1.ReasonHelmReleaseFailed,
			Message: "Failed to create helm release",
			Err:     out.Err,
		})
	}

	state := conditionState{InternalObjectState: out.Internal}

	if state.Ready() && !out.ChartDeployed {
		// A chart-version change moves the artifact without touching the HelmRelease's spec.
		return conditionState{InternalObjectState: internalobj.InternalObjectState{
			Status: metav1.ConditionUnknown,
			Reason: helmv1alpha1.ReasonReconciling,
		}}
	}

	return state
}

type lifecycleReasonSet struct {
	succeeded, inFlight, failed string
}

var lifecycleReasons = map[string]lifecycleReasonSet{
	helmv1alpha1.ConditionTypeInstalled: {
		helmv1alpha1.ReasonInstallSucceeded, helmv1alpha1.ReasonInstalling, helmv1alpha1.ReasonInstallFailed,
	},
	helmv1alpha1.ConditionTypeUpdateInstalled: {
		helmv1alpha1.ReasonUpgradeSucceeded, helmv1alpha1.ReasonUpgrading, helmv1alpha1.ReasonUpgradeFailed,
	},
	helmv1alpha1.ConditionTypeConfigurationApplied: {
		helmv1alpha1.ReasonValuesApplied, helmv1alpha1.ReasonApplyingValues, helmv1alpha1.ReasonValuesApplyFailed,
	},
}

func lifecycleState(conditionType string, state conditionState) conditionState {
	reasons, ok := lifecycleReasons[conditionType]
	if !ok {
		return state
	}

	switch {
	case state.Status == metav1.ConditionTrue:
		state.Reason = reasons.succeeded
	case state.Status == metav1.ConditionUnknown:
		state.Reason = reasons.inFlight
	case state.Status == metav1.ConditionFalse && state.Reason == helmv1alpha1.ReasonReleaseFailed:
		state.Reason = reasons.failed
	}

	return state
}

// maintenanceActiveState is the verdict of a release the controller has stopped reconciling.
// It is written to Managed and to Ready alike.
func maintenanceActiveState() conditionState {
	return conditionState{InternalObjectState: internalobj.InternalObjectState{
		Observed: true,
		Status:   metav1.ConditionFalse,
		Reason:   helmv1alpha1.ReasonMaintenanceModeActive,
		Message:  "Maintenance mode enabled",
	}}
}

// maintenanceLiftedState is the verdict of a release that is managed again. It reaches
// Managed alone; what the resumed pass makes of the chart is Ready's answer.
func maintenanceLiftedState() conditionState {
	return conditionState{InternalObjectState: internalobj.InternalObjectState{
		Observed: true,
		Status:   metav1.ConditionTrue,
		Reason:   helmv1alpha1.ReasonMaintenanceModeInactive,
		Message:  "Maintenance mode disabled",
	}}
}

func failureState(failure Failure) conditionState {
	return conditionState{
		InternalObjectState: internalobj.InternalObjectState{
			Observed: true,
			Status:   metav1.ConditionFalse,
			Reason:   failure.Reason,
			Message:  failure.Message,
		},
		Failure: failure,
	}
}

func reported(state conditionState) *Failure {
	if state.Failure.Err == nil {
		return nil
	}

	return &state.Failure
}

// retryErr hands the cause of a failure to the work queue: the first retryable one of every
// failure the pass produced, in the order the steps ran.
func retryErr(failures ...Failure) error {
	for _, failure := range failures {
		if failure.Terminal || failure.AwaitsWatch || failure.BadInput {
			continue
		}

		if failure.Err != nil {
			return failure.Err
		}
	}

	return nil
}

// artifactRevision is the revision the release would be installed from, whether that artifact
// is the one the current spec asks for, and whether there is an artifact at all.
func artifactRevision(src InternalSource) (revision string, current, ok bool) {
	switch source := src.(type) {
	case ServedByHelmChart:
		if source.Result.Artifact == nil {
			return "", false, false
		}

		return source.Result.Artifact.Revision, source.Result.Internal.Ready(), true
	case ServedByOCIRepository:
		if source.Result.Artifact == nil {
			return "", false, false
		}

		return source.Result.Artifact.Revision, source.Result.Internal.Ready(), true
	default:
		return "", false, false
	}
}

// releaseReported is the internal HelmRelease's own verdict, before the chart provenance
// evaluateRelease folds into it: it observed the spec that was written and is ready for it.
func releaseReported(out *internalobj.ReleaseOutcome) bool {
	return out != nil && out.Internal.Ready()
}

// valuesDeployed reports that the values the spec asks for are the ones the release is
// running, by matching the digest on the latest history entry.
func valuesDeployed(outcome ChartReached) bool {
	if outcome.Release == nil {
		return false
	}

	// Latest is the highest revision whatever became of it, so it must also be deployed.
	latest := outcome.Release.History.Latest()

	return latest != nil && latest.Status == "deployed" && latest.ConfigDigest == outcome.ValuesDigest
}

func probeRequeueAfter(src InternalSource) time.Duration {
	source, ok := src.(ServedByOCIRepository)
	if !ok {
		return 0
	}

	return source.Result.ProbeRequeueAfter
}

func (d *Decision) set(in Inputs, conditionType string, state conditionState) {
	if state.Status == "" || state.Reason == "" {
		return
	}

	apimeta.SetStatusCondition(&d.Conditions, metav1.Condition{
		Type:               conditionType,
		Status:             state.Status,
		Reason:             state.Reason,
		Message:            state.Message,
		ObservedGeneration: in.Generation,
		LastTransitionTime: metav1.NewTime(in.Now),
	})
}

// setAbnormal raises an abnormal-true condition and drops it from RemoveConditions, so the
// two do not contradict each other.
func (d *Decision) setAbnormal(in Inputs, conditionType string, failure Failure) {
	d.RemoveConditions = slices.DeleteFunc(d.RemoveConditions, func(v string) bool { return v == conditionType })

	apimeta.SetStatusCondition(&d.Conditions, metav1.Condition{
		Type:               conditionType,
		Status:             metav1.ConditionTrue,
		Reason:             failure.Reason,
		Message:            failure.Message,
		ObservedGeneration: in.Generation,
		LastTransitionTime: metav1.NewTime(in.Now),
	})
}
