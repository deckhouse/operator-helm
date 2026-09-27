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
	"time"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
	"github.com/deckhouse/operator-helm/internal/internalobj"
)

// Failure is one way a pass can fail, named the way the release reports it. Reason
// and Message are what the condition carries; Err is logged and, unless the failure
// is terminal, handed to the work queue's rate limiter.
type Failure struct {
	Reason  string
	Message string
	Err     error

	// Terminal marks a failure nothing this controller can do will resolve: the
	// release's own spec is at fault, or something outside it has to be changed by
	// hand. It is reported as Stalled, the kstatus condition for exactly that, and
	// it excludes Retry — handing such a failure to the work queue would burn the
	// rate limiter on an attempt whose outcome is already known.
	Terminal bool
	// AwaitsWatch marks a failure something else already brings the release back
	// from: a watch on the object the step needed fires when it appears. It keeps Err
	// away from the work queue, because retrying on a rate limiter would only
	// rediscover an absence until the watch reports it gone. Everything else is
	// retried — a step that failed on the way to a write left nothing for a watch to
	// fire on.
	AwaitsWatch bool
	// RetryAfter schedules the next pass itself, for a failure that neither a watch
	// nor the work queue covers. Waiting on the repository's first sync is the case:
	// the catalog object of the chart may never appear — a mistyped name has none —
	// and the sync time lands on the repository's status, which the release's own
	// watch on the repository filters out by generation. Without a timer the release
	// would wait for an event nobody sends.
	RetryAfter time.Duration
	// BadInput marks a failure of the spec the release was given rather than of the
	// work towards it: what the spec asks for is not there. It is not Terminal — the
	// thing asked for may be published later, and a watch brings the release back
	// when it is — but it is not progress either, so the release reports it without
	// Reconciling and without handing it to the work queue: nothing about a retry
	// changes what the spec asks for.
	//
	// The release is not settled either, so observedGeneration keeps lagging and a
	// kstatus reader still calls it InProgress. That vocabulary has no term for
	// "waiting on something the spec named and the repository does not have", and
	// claiming Stalled instead would be worse: Stalled promises that nothing but a
	// new spec can move the object, and here a publish to the repository can.
	BadInput bool
	// RequeueAfter schedules another pass on a timer, for a cause no watch covers.
	RequeueAfter time.Duration
}

// Inputs carries everything Evaluate needs. It holds no clients and no clock: Now,
// the current conditions and the outcome of the pass are supplied by the caller, so
// the function is deterministic and unit testable. It is the release-side
// counterpart of the repository package's Inputs.
type Inputs struct {
	Generation         int64
	ObservedGeneration int64
	Now                time.Time

	// ConditionTypes is the set the pass's verdict is projected onto. It is derived
	// from the release's own status, which is why the caller computes it.
	ConditionTypes []string

	// Outcome is how the pass ended. Exactly one of the four is what happened, and
	// each carries only what that ending can say anything about.
	Outcome Outcome

	// MaintenanceLifted reports that this pass took the release out of maintenance
	// mode and went on with the work maintenance was holding back. It is not an
	// outcome: the pass does not end there, it ends wherever the resumed work ends,
	// and Managed is the one thing the lift itself has to say.
	MaintenanceLifted bool
}

// Outcome is how one reconcile pass ended. The interface is sealed: the four
// implementations below are the four endings a pass has, and a pass cannot end in
// two of them at once.
type Outcome interface{ outcome() }

// StepFailed is a failure of one of the steps that run before the chart is
// resolved. It owns Ready alone, because the pass never got far enough to say
// anything about installing or configuring. Whether the failure is terminal is
// carried by the Failure itself and adds Stalled.
type StepFailed struct{ Failure Failure }

// MaintenanceChanged is the pass that moved the release into maintenance mode, or
// failed to move it either way. Such a pass reports nothing else: the chart is not
// touched. Lifting maintenance successfully is not one of these endings — the
// release is managed again, so that pass carries on to the chart and says so
// through Inputs.MaintenanceLifted.
type MaintenanceChanged struct {
	Result internalobj.MaintenanceOutcome
}

// InMaintenance is the pass that found the release already in maintenance and left
// it there. Nothing ran, and the release's spec may have changed since.
type InMaintenance struct{}

// ChartReached is the pass that got as far as the chart. Release is nil when the
// pass never reached the internal HelmRelease, which is every pass whose source did
// not produce an artifact.
type ChartReached struct {
	// Forced reports whether this pass was requested through the force reconcile
	// annotation. It belongs here alone: the other three endings either cannot act on
	// a force request or deliberately discard it.
	Forced bool

	Internal InternalSource
	Release  *internalobj.ReleaseOutcome

	// ChartInfoOutdated and ValuesDigest are the two facts the record of what was
	// last applied turns on. ValuesDigest is the digest of the values the spec asks
	// for, which the deployed revision is compared against.
	ChartInfoOutdated bool
	ValuesDigest      string
}

// InternalSource is which internal source serves the release, or the failure to
// decide. The interface is sealed: a release is served by an internal HelmChart or
// by an internal OCIRepository, never both.
type InternalSource interface{ internalSource() }

// SourceUnresolved stands in for the source outcome when the pass could not decide
// which internal source the release needs. It is projected across ConditionTypes
// exactly as an outcome would be.
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

// Decision is the full desired status plus the scheduling verdict. Removing a
// condition is expressed by its presence in RemoveConditions; the field writes are
// asked for rather than performed, because they are reached through the release
// adapter and Evaluate holds no adapter.
type Decision struct {
	Conditions       []metav1.Condition
	RemoveConditions []string

	// ObservedGeneration is nil on a pass that has not finished with the spec it was
	// given, which is every pass that left something to come back to: a step that will
	// be retried, an internal object still working, a release sitting in maintenance.
	// Saying the spec was observed while the release is still on its way to a verdict
	// about it is what would make kstatus call such a release settled.
	ObservedGeneration *int64

	// ForceReconcileTime records that a force request was acted on, not that it
	// succeeded: the outcome is carried by Ready.
	ForceReconcileTime *metav1.Time
	// Reported is the failure behind the verdict this pass wrote, when there is one.
	// It is carried out rather than logged where it happened because most release
	// failures never reach the work queue: several of them report a fixed message,
	// so unless the pass names the cause on its way out it is named nowhere.
	Reported *Failure
	// ConsumeForce asks for the force annotation to be removed. Only a pass that
	// acted on the request, or one that deliberately discarded it, may take it away:
	// a pass that failed before the request could be honoured has to leave it for the
	// pass that can. Lifting maintenance is the clearest case — the request becomes
	// actionable on the very next pass.
	ConsumeForce bool
	// ApplyChart and ApplyValues ask for the record of what was last applied to be
	// advanced to what the spec asks for.
	ApplyChart  bool
	ApplyValues bool

	RequeueAfter time.Duration
	Err          error
}

// conditionState is one verdict: the value a condition would carry if this were the
// thing the release reports. It is what Evaluate compares and projects. An internal
// object's own state is already that verdict, so it is carried whole; Failure is
// what sits behind a verdict the pass produced itself, and it is carried whole too —
// its error is what the pass reports, and its flags are what decides whether
// anything comes back to it.
type conditionState struct {
	internalobj.InternalObjectState

	Failure Failure
}

// Evaluate derives the desired release status from the results of a single
// reconcile pass.
func Evaluate(in Inputs) Decision {
	decision := evaluate(in)

	if in.MaintenanceLifted {
		// The suspend flag came off the internal HelmRelease, which is the only thing
		// that makes the release managed again, so Managed is written whatever the
		// resumed work went on to end in. Ready is left to that work: the release is
		// managed long before it is ready.
		decision.set(in, helmv1alpha1.ConditionTypeManaged, maintenanceLiftedState())
	}

	decision.ObservedGeneration = observedGeneration(in, decision)

	return decision
}

// observedGeneration decides whether this pass may claim to have finished with the
// spec it was given. Only a verdict the release will keep counts: one it settled on,
// and one it gave up on for good. Anything else — a step to be retried, an internal
// object still working — has something to come back to, and a reader comparing the
// generation against metadata.generation is entitled to read the difference as work
// still in flight rather than as a verdict about the spec in front of it.
func observedGeneration(in Inputs, decision Decision) *int64 {
	settled := apimeta.IsStatusConditionTrue(decision.Conditions, helmv1alpha1.ConditionTypeReady) ||
		apimeta.IsStatusConditionTrue(decision.Conditions, helmv1alpha1.ConditionTypeStalled)
	if !settled {
		return nil
	}

	observed := max(in.Generation, in.ObservedGeneration)

	return &observed
}

func evaluate(in Inputs) Decision {
	decision := Decision{
		// Stalled and Reconciling are the two kstatus abnormal-true conditions: by
		// that convention each is present only while it applies, so every pass takes
		// both away unless it has a reason to write one. A release that recovers must
		// not keep reporting that it cannot, and one that settled must not keep
		// reporting work in flight.
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
		// The force request is dropped, not processed: the stamp means the latter, so
		// it stays untouched, while the request itself is taken away — replaying one
		// made days earlier the moment maintenance is lifted would surprise whoever
		// lifted it. Reconciling goes with it, which RemoveConditions already
		// arranges: a release in maintenance has nothing in flight to report.
		decision.ConsumeForce = true
		// The verdict is restated rather than left where the pass that entered
		// maintenance put it: a release the controller has stopped reconciling is not a
		// ready release, and Ready is what a reader goes by.
		decision.set(in, helmv1alpha1.ConditionTypeManaged, maintenanceActiveState())
		decision.set(in, helmv1alpha1.ConditionTypeReady, maintenanceActiveState())

		return decision
	case ChartReached:
		return evaluateChartReached(in, decision, outcome)
	default:
		// Unreachable by construction, and silence is the one thing it must not be
		// met with: an empty decision writes no status at all, so a pass that ended in
		// no outcome would leave the release reporting whatever it reported before,
		// indefinitely. The error goes to the work queue instead, which logs it and
		// brings the release back with backoff.
		return Decision{Err: fmt.Errorf("reconcile pass ended in no outcome: %T", in.Outcome)}
	}
}

func evaluateStepFailed(in Inputs, decision Decision, outcome StepFailed) Decision {
	decision.set(in, helmv1alpha1.ConditionTypeReady, failureState(outcome.Failure))

	switch {
	case outcome.Failure.Terminal:
		decision.setAbnormal(in, helmv1alpha1.ConditionTypeStalled, outcome.Failure)
		// Dropped rather than processed, as a release settling into maintenance drops
		// one: the stamp means the request was acted on, so it stays untouched, while
		// the request itself is taken away. A terminal failure leaves no later pass to
		// hand it to, and the annotation is matched on presence alone — leaving it
		// behind would make the next force request change nothing about the object and
		// so fire no event at all, which is exactly how someone who corrected the cause
		// would ask to be retried.
		decision.ConsumeForce = true
	case outcome.Failure.AwaitsWatch:
		// Nothing is scheduled for this one: the failure is kept off the work queue
		// because a watch, or the timer the step asked for, brings the release back when
		// what it waits for arrives. Saying "retrying" here would promise an attempt
		// nobody made. The plain Reconciling reason keeps the release readable as work
		// still in flight — which it is, from outside — without that promise.
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
		// Managed is left exactly where it was. It reports whether the internal
		// HelmRelease is suspended, and this attempt did not change that; a Managed
		// written from a failed attempt would be read as a release resting in
		// maintenance, which is the one state the controller never comes back to on its
		// own. Reported as any other step that failed: not ready, retried.
		return evaluateStepFailed(in, decision, StepFailed{Failure{
			Reason:  helmv1alpha1.ReasonFailed,
			Message: "Failed to change maintenance mode",
			Err:     outcome.Result.Err,
		}})
	}

	if !outcome.Result.Activated {
		// Unreachable: a pass that lifted maintenance resumes the work instead of
		// ending here, and it reports the lift through Inputs.MaintenanceLifted. Writing
		// a status for it here is what must not happen — "managed again" with nothing
		// else said reads as a settled release whose chart was never deployed.
		return Decision{Err: errors.New("reconcile pass ended on lifted maintenance")}
	}

	decision.set(in, helmv1alpha1.ConditionTypeManaged, maintenanceActiveState())
	decision.set(in, helmv1alpha1.ConditionTypeReady, maintenanceActiveState())
	// Settling into maintenance drops an in-flight force request for the same reason
	// a release already in it does: the release will not act on the request while it
	// is held, and leaving Reconciling behind would report work in flight to kstatus
	// forever.
	decision.ConsumeForce = true

	return decision
}

func evaluateChartReached(in Inputs, decision Decision, outcome ChartReached) Decision {
	chart, release := evaluateChart(outcome.Internal), evaluateRelease(outcome.Release)

	var stalled bool

	verdict, found := decide(chart, release)
	if found {
		for _, conditionType := range in.ConditionTypes {
			decision.set(in, conditionType, verdict)
		}

		decision.Reported = reported(verdict)
	}

	oci, servedByOCI := outcome.Internal.(ServedByOCIRepository)

	switch {
	case servedByOCI && oci.Result.ProbeTerminal:
		// The registry rejected the request, or what it serves is not a chart. The
		// verdict is about the artifact, so no watch and no timer brings it back: the
		// catalog has to publish something else, or the repository has to be fixed.
		stalled = true

		decision.setAbnormal(in, helmv1alpha1.ConditionTypeStalled, Failure{
			Reason:  oci.Result.ProbeReason,
			Message: oci.Result.ProbeMessage,
		})
	case found && verdict.Stalled:
		// The internal object gave up on the spec it was given, the way the internal
		// repository does for its own kind. It has stopped acting on that spec, so it
		// goes quiet and the verdict cannot change: only a new spec — a values or
		// version edit here — gives it something else to act on. Reported with the
		// verdict's own reason, which the error rules made specific, rather than with
		// the internal object's count of spent attempts.
		stalled = true

		decision.setAbnormal(in, helmv1alpha1.ConditionTypeStalled, Failure{
			Reason:  verdict.Reason,
			Message: verdict.Message,
		})
	}

	if progress, ok := evaluateReconciling(verdict, found, stalled); ok {
		decision.setAbnormal(in, helmv1alpha1.ConditionTypeReconciling, progress)
	}

	if outcome.Forced {
		decision.ForceReconcileTime = &metav1.Time{Time: in.Now}
		decision.ConsumeForce = true
		// Reconciling is not taken away here. The pass the force request asked for
		// published it before the work began, and whether it stays is the same
		// question as on any other pass: it stays while the rollout it kicked off is
		// still running, and the pass that sees the release settle removes it.
	}

	_, installable := artifactRevision(outcome.Internal)
	decision.ApplyChart = installable && release.Ready() && outcome.ChartInfoOutdated
	decision.ApplyValues = release.Ready() && valuesDeployed(outcome)
	decision.RequeueAfter = probeRequeueAfter(outcome.Internal)
	if decision.RequeueAfter == 0 {
		decision.RequeueAfter = verdict.Failure.RetryAfter
	}

	if !stalled && decision.RequeueAfter == 0 {
		// A verdict this pass produced itself — a write to an internal object that did
		// not land, a source that could not be resolved — is the one failure here that
		// nothing else comes back to: the object a watch would fire on was left exactly
		// as it was, so the work queue's rate limiter is all there is. An internal object
		// that exists and is merely not ready yet carries no error at all and is woken by
		// the watch on it, which is why it never reaches this. The two exclusions above
		// are already scheduled: a stalled verdict waits for a new spec, and a failure the
		// artifact probe put on a timer would lose that timer, because an error returned
		// beside a requeue takes precedence over it.
		decision.Err = retryErr(verdict.Failure)
	}

	return decision
}

// evaluateReconciling decides the kstatus progress condition, the way the repository
// package's counterpart of this name does: it is raised while the pass has left
// something to wait for, and it is absent the moment there is nothing. The verdict
// the release reports is the whole input, because that verdict already is "how far
// this pass got".
//
// A terminal failure outranks it, as Stalled outranks Reconciling there: a release
// that cannot proceed is not making progress, and an internal object that gave up is
// exactly such a release — it has stopped acting on the spec it was given.
// Unknown is work genuinely in flight — an internal object still reconciling the spec
// it was given, or one that has not reported on it yet — and the object's own words
// are the most specific thing to show for it. False is a failure that is not
// terminal, so something will come back to it: the watch on the internal object, the
// probe's timer, or the work queue.
//
// A bad input outranks it for the same reason a terminal failure does, without being
// one: no pass is on its way to what the spec asks for, because what it asks for is
// not there. Promising a retry would tell whoever mistyped a chart version that the
// controller is working on it. It is checked before the wait below for the same
// reason it is checked beside stalled: a spec that asks for something absent is not
// waiting for anything, whatever else the failure carries.
func evaluateReconciling(verdict conditionState, found, stalled bool) (Failure, bool) {
	if stalled || !found || verdict.Failure.BadInput {
		return Failure{}, false
	}

	switch verdict.Status {
	case metav1.ConditionUnknown:
		return Failure{Reason: verdict.Reason, Message: verdict.Message}, true
	case metav1.ConditionFalse:
		if verdict.Failure.AwaitsWatch {
			// The same honesty a step failure awaiting a watch gets, for the verdict this
			// pass produced itself: the source could not be resolved because the repository
			// has yet to sync, and a watch or the timer the step asked for brings the release
			// back when it has. No pass is retrying anything in the meantime, so the plain
			// Reconciling reason reports work still in flight without naming an attempt
			// nobody made. Only a failure says this — an internal object that is merely not
			// ready carries none, and keeps its own words.
			return Failure{
				Reason:  helmv1alpha1.ReasonReconciling,
				Message: "Waiting for what the failed step needs",
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

// decide picks the one verdict the release reports, the way a reader of the status
// would: the first step that did not succeed is the answer, and when every step
// succeeded the last one is. An empty verdict is not an answer — a pass may touch
// only one of the two internal objects.
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
		// The version is still recorded — that is what keeps this release
		// reconcilable — but the repository no longer offers the tag, so the pull
		// cannot succeed. Name that cause instead of leaving only the source
		// controller's "not found".
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
			Reason:  helmv1alpha1.ReasonReleaseFailed,
			Message: "Failed to create helm release",
			Err:     out.Err,
		})
	}

	state := conditionState{InternalObjectState: out.Internal}

	if state.Ready() && !out.ChartDeployed {
		// The HelmRelease still reports the readiness of the previous revision: a
		// chart-version change moves the artifact without touching its spec. Hold the
		// verdict at Reconciling so the record of what was applied, and the conditions
		// projected from here, do not advance ahead of the rollout.
		return conditionState{InternalObjectState: internalobj.InternalObjectState{
			Status: metav1.ConditionUnknown,
			Reason: helmv1alpha1.ReasonReconciling,
		}}
	}

	return state
}

// maintenanceActiveState is the verdict of a release the controller has stopped
// reconciling. It is written to Managed and to Ready alike: a release whose deployed
// resources are left alone is not one whose spec is being kept true.
func maintenanceActiveState() conditionState {
	return conditionState{InternalObjectState: internalobj.InternalObjectState{
		Observed: true,
		Status:   metav1.ConditionFalse,
		Reason:   helmv1alpha1.ReasonMaintenanceModeActive,
		Message:  "Maintenance mode enabled",
	}}
}

// maintenanceLiftedState is the verdict of a release that is managed again. It
// reaches Managed alone; what the resumed pass makes of the chart is Ready's answer.
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

// retryErr hands the cause of a failure to the work queue. A failed cluster read or
// cluster write leaves nothing behind for a watch to fire on — the object it would
// have written was not written — so the rate limiter is what brings the release
// back. The exceptions say so themselves: a terminal failure retrying cannot change,
// one the release is woken from by a watch, and one whose cause is the spec itself. A
// failure carrying no error of its own rides the timer the step asked for instead.
func retryErr(failure Failure) error {
	if failure.Terminal || failure.AwaitsWatch || failure.BadInput {
		return nil
	}

	return failure.Err
}

// artifactRevision is the revision the release would be installed from, and whether
// there is one at all. An artifact only counts once the object that produced it has
// reported on the spec it was given: until then the revision on it is the previous
// one, and installing from it would roll the release back to the chart it is
// already running.
func artifactRevision(src InternalSource) (string, bool) {
	switch source := src.(type) {
	case ServedByHelmChart:
		if source.Result.Artifact == nil || !source.Result.Internal.Observed {
			return "", false
		}

		return source.Result.Artifact.Revision, true
	case ServedByOCIRepository:
		if source.Result.Artifact == nil || !source.Result.Internal.Observed {
			return "", false
		}

		return source.Result.Artifact.Revision, true
	default:
		return "", false
	}
}

func valuesDeployed(outcome ChartReached) bool {
	if outcome.Release == nil {
		return false
	}

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

// setAbnormal raises an abnormal-true condition, which by the kstatus convention is
// present only while it applies. The condition is dropped from RemoveConditions so
// the two do not contradict each other.
func (d *Decision) setAbnormal(in Inputs, conditionType string, failure Failure) {
	d.RemoveConditions = slicesDelete(d.RemoveConditions, conditionType)

	apimeta.SetStatusCondition(&d.Conditions, metav1.Condition{
		Type:               conditionType,
		Status:             metav1.ConditionTrue,
		Reason:             failure.Reason,
		Message:            failure.Message,
		ObservedGeneration: in.Generation,
		LastTransitionTime: metav1.NewTime(in.Now),
	})
}

func slicesDelete(values []string, value string) []string {
	kept := values[:0]
	for _, v := range values {
		if v != value {
			kept = append(kept, v)
		}
	}

	return kept
}
