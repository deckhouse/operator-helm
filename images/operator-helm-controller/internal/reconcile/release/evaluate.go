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
	"time"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
	"github.com/deckhouse/operator-helm/internal/services"
)

// Failure is one way a pass can fail, named the way the release reports it. Reason
// and Message are what the condition carries; Err is logged and, when Retry is set,
// handed to the work queue's rate limiter.
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
	// Retry hands Err to the work queue when the pass ends. Without it the failure
	// is reported and the release waits for a watch to wake it.
	Retry bool
	// RequeueAfter schedules another pass on a timer, for a cause no watch covers.
	RequeueAfter time.Duration
}

// Inputs carries everything Evaluate needs. It holds no clients and no clock: Now,
// the current conditions and the derived facts about the release are supplied by the
// caller, so the function is deterministic and unit testable. It is the release-side
// counterpart of the repository package's Inputs.
//
// The failure fields differ in how far the verdict reaches, not in severity. Step is
// a failure of one of the steps that run before the chart is resolved; it owns Ready
// alone, because the pass never got far enough to say anything about installing or
// configuring. ChartSource stands in for the chart outcome when the pass could not
// decide which internal source the release needs, so it is projected across
// ConditionTypes exactly as an outcome would be. Whether a failure is terminal is
// orthogonal to either: it is carried by the Failure itself and adds Stalled.
type Inputs struct {
	Generation         int64
	ObservedGeneration int64
	Now                time.Time

	// ConditionTypes is the set the pass's verdict is projected onto. It is derived
	// from the release's own status, which is why the caller computes it.
	ConditionTypes []string

	// Forced reports whether this pass was requested through the force reconcile
	// annotation.
	Forced bool

	Step        *Failure
	ChartSource *Failure

	// Maintenance is set on the pass that moved the release into or out of
	// maintenance mode. Such a pass reports nothing else: the chart is not touched.
	Maintenance *services.MaintenanceOutcome
	// DiscardForce asks for an in-flight force request to be dropped. A release
	// sitting in maintenance can never act on one, and leaving Reconciling behind
	// would report work in flight to kstatus forever.
	DiscardForce bool

	// Chart and OCIRepo are the two internal source kinds, and they are mutually
	// exclusive: a release is served by an internal HelmChart or by an internal
	// OCIRepository, never both.
	Chart   *services.ChartOutcome
	OCIRepo *services.OCIRepoOutcome
	Release *services.ReleaseOutcome

	// ChartInfoOutdated and ValuesDigest are the two facts the record of what was
	// last applied turns on. ValuesDigest is the digest of the values the spec asks
	// for, which the deployed revision is compared against.
	ChartInfoOutdated bool
	ValuesDigest      string
}

// Decision is the full desired status plus the scheduling verdict. Removing a
// condition is expressed by its presence in RemoveConditions; the field writes are
// asked for rather than performed, because they are reached through the release
// adapter and Evaluate holds no adapter.
type Decision struct {
	Conditions       []metav1.Condition
	RemoveConditions []string

	// ObservedGeneration is nil on a pass that reported nothing about the spec it
	// was given. A release sitting in maintenance is the case: its spec may have
	// changed since, and saying the change was observed while nothing was done about
	// it is what would make kstatus call such a release settled.
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
// object's own state is already that verdict, so it is carried whole; Err is what
// sits behind a verdict the pass produced itself.
type conditionState struct {
	services.InternalObjectState

	Err error
}

// Evaluate derives the desired release status from the results of a single
// reconcile pass.
func Evaluate(in Inputs) Decision {
	observed := max(in.Generation, in.ObservedGeneration)

	decision := Decision{
		ObservedGeneration: &observed,
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

	switch {
	case in.Step != nil:
		decision.set(in, helmv1alpha1.ConditionTypeReady, failureState(*in.Step))
		if in.Step.Terminal {
			decision.setAbnormal(in, helmv1alpha1.ConditionTypeStalled, *in.Step)
			// Dropped rather than processed, as a release settling into maintenance
			// drops one: the stamp means the request was acted on, so it stays
			// untouched, while the request itself is taken away. A terminal failure
			// leaves no later pass to hand it to, and the annotation is matched on
			// presence alone — leaving it behind would make the next force request
			// change nothing about the object and so fire no event at all, which is
			// exactly how someone who corrected the cause would ask to be retried.
			decision.ConsumeForce = true
		} else {
			decision.setAbnormal(in, helmv1alpha1.ConditionTypeReconciling, Failure{
				Reason:  helmv1alpha1.ReasonProgressingWithRetry,
				Message: "Retrying the step that failed",
			})
		}
		decision.Reported = in.Step
		decision.Err = retryErr(*in.Step)
		decision.RequeueAfter = in.Step.RequeueAfter

		return decision
	case in.Maintenance != nil:
		state := maintenanceState(*in.Maintenance)
		decision.set(in, helmv1alpha1.ConditionTypeManaged, state)
		decision.set(in, helmv1alpha1.ConditionTypeReady, state)
		decision.Reported = reported(state)
	}

	if in.DiscardForce {
		// Dropped, not processed: the stamp means the latter, so it stays untouched,
		// while the request itself is taken away — replaying one made days earlier the
		// moment maintenance is lifted would surprise whoever lifted it. Reconciling
		// goes with it, which the default above already arranges: a release in
		// maintenance has nothing in flight to report.
		decision.ConsumeForce = true

		if in.Maintenance == nil {
			// Nothing ran in this pass: the release was already in maintenance and
			// stays there, whatever its spec now says.
			decision.ObservedGeneration = nil
		}

		return decision
	}

	if in.Maintenance != nil {
		return decision
	}

	chart, release := evaluateChart(in), evaluateRelease(in)

	var stalled bool

	verdict, found := decide(chart, release)
	if found {
		for _, conditionType := range in.ConditionTypes {
			decision.set(in, conditionType, verdict)
		}

		decision.Reported = reported(verdict)
	}

	switch {
	case in.OCIRepo != nil && in.OCIRepo.ProbeTerminal:
		// The registry rejected the request, or what it serves is not a chart. The
		// verdict is about the artifact, so no watch and no timer brings it back: the
		// catalog has to publish something else, or the repository has to be fixed.
		stalled = true

		decision.setAbnormal(in, helmv1alpha1.ConditionTypeStalled, Failure{
			Reason:  in.OCIRepo.ProbeReason,
			Message: in.OCIRepo.ProbeMessage,
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

	if in.Forced {
		decision.ForceReconcileTime = &metav1.Time{Time: in.Now}
		decision.ConsumeForce = true
		// Reconciling is not taken away here. The pass the force request asked for
		// published it before the work began, and whether it stays is the same
		// question as on any other pass: it stays while the rollout it kicked off is
		// still running, and the pass that sees the release settle removes it.
	}

	_, installable := artifactRevision(in)
	decision.ApplyChart = installable && release.Ready() && in.ChartInfoOutdated
	decision.ApplyValues = release.Ready() && valuesDeployed(in)
	decision.RequeueAfter = probeRequeueAfter(in)

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
func evaluateReconciling(verdict conditionState, found, stalled bool) (Failure, bool) {
	if stalled || !found {
		return Failure{}, false
	}

	switch verdict.Status {
	case metav1.ConditionUnknown:
		return Failure{Reason: verdict.Reason, Message: verdict.Message}, true
	case metav1.ConditionFalse:
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
// The two kinds are mutually exclusive: a release is served by an internal HelmChart
// or by an internal OCIRepository, never both.
func evaluateChart(in Inputs) conditionState {
	switch {
	case in.ChartSource != nil:
		return failureState(*in.ChartSource)
	case in.Chart != nil:
		if in.Chart.Err != nil {
			return failureState(Failure{
				Reason:  helmv1alpha1.ReasonHelmChartFailed,
				Message: "Failed to create helm chart",
				Err:     in.Chart.Err,
			})
		}

		return conditionState{InternalObjectState: in.Chart.Internal}
	case in.OCIRepo != nil:
		return evaluateOCIRepo(*in.OCIRepo)
	default:
		return conditionState{}
	}
}

func evaluateOCIRepo(out services.OCIRepoOutcome) conditionState {
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

func evaluateRelease(in Inputs) conditionState {
	if in.Release == nil {
		return conditionState{}
	}

	if in.Release.Err != nil {
		return failureState(Failure{
			Reason:  helmv1alpha1.ReasonReleaseFailed,
			Message: "Failed to create helm release",
			Err:     in.Release.Err,
		})
	}

	state := conditionState{InternalObjectState: in.Release.Internal}

	if state.Ready() && !in.Release.ChartDeployed {
		// The HelmRelease still reports the readiness of the previous revision: a
		// chart-version change moves the artifact without touching its spec. Hold the
		// verdict at Reconciling so the record of what was applied, and the conditions
		// projected from here, do not advance ahead of the rollout.
		return conditionState{InternalObjectState: services.InternalObjectState{
			Status: metav1.ConditionUnknown,
			Reason: helmv1alpha1.ReasonReconciling,
		}}
	}

	return state
}

func maintenanceState(out services.MaintenanceOutcome) conditionState {
	if out.Err != nil {
		return failureState(Failure{
			Reason:  helmv1alpha1.ReasonFailed,
			Message: "Failed to change maintenance mode",
			Err:     out.Err,
		})
	}

	if out.Activated {
		return conditionState{InternalObjectState: services.InternalObjectState{
			Observed: true,
			Status:   metav1.ConditionFalse,
			Reason:   helmv1alpha1.ReasonMaintenanceModeActive,
			Message:  "Maintenance mode enabled",
		}}
	}

	return conditionState{InternalObjectState: services.InternalObjectState{
		Observed: true,
		Status:   metav1.ConditionTrue,
		Reason:   helmv1alpha1.ReasonMaintenanceModeInactive,
		Message:  "Maintenance mode disabled",
	}}
}

func failureState(failure Failure) conditionState {
	return conditionState{
		InternalObjectState: services.InternalObjectState{
			Observed: true,
			Status:   metav1.ConditionFalse,
			Reason:   failure.Reason,
			Message:  failure.Message,
		},
		Err: failure.Err,
	}
}

// reported turns the verdict into the failure the pass logs, if it failed at all.
func reported(state conditionState) *Failure {
	if state.Err == nil {
		return nil
	}

	return &Failure{Reason: state.Reason, Message: state.Message, Err: state.Err}
}

func retryErr(failure Failure) error {
	if !failure.Retry || failure.Terminal {
		return nil
	}

	return failure.Err
}

// artifactRevision is the revision the release would be installed from, and whether
// there is one at all. The two internal source kinds are mutually exclusive — a pass
// reconciles one or the other — so which one answered is not asked, and a failed
// pass leaves no artifact behind to be mistaken for one.
func artifactRevision(in Inputs) (string, bool) {
	switch {
	case in.Chart != nil && in.Chart.Artifact != nil && in.Chart.Internal.Observed:
		return in.Chart.Artifact.Revision, true
	case in.OCIRepo != nil && in.OCIRepo.Artifact != nil && in.OCIRepo.Internal.Observed:
		return in.OCIRepo.Artifact.Revision, true
	default:
		return "", false
	}
}

// valuesDeployed reports whether the revision the release currently has deployed was
// installed with the values the spec asks for.
func valuesDeployed(in Inputs) bool {
	if in.Release == nil {
		return false
	}

	latest := in.Release.History.Latest()

	return latest != nil && latest.Status == "deployed" && latest.ConfigDigest == in.ValuesDigest
}

func probeRequeueAfter(in Inputs) time.Duration {
	if in.OCIRepo == nil {
		return 0
	}

	return in.OCIRepo.ProbeRequeueAfter
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
