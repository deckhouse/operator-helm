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

package helmclusteraddonrepository

import (
	"fmt"
	"math/rand/v2"
	"time"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
	"github.com/deckhouse/operator-helm/internal/services"
)

// Inputs carries everything Evaluate needs. It holds no clients and no clock:
// Now and Jitter are supplied by the caller so the function is deterministic
// and unit testable.
type Inputs struct {
	Generation int64
	Now        time.Time
	Jitter     float64
	Current    helmv1alpha1.HelmClusterAddonRepositoryStatus

	SecretsErr            error
	InternalRepositoryErr error
	InternalRepository    services.InternalRepositoryState
	ConfigErr             *services.ConfigOutcome

	// Attempted reports whether a synchronization attempt ran in this pass.
	// Fetch and Catalog are nil when it did not.
	Attempted bool
	Fetch     *services.FetchOutcome
	Catalog   *services.CatalogOutcome
}

// Decision is the full desired status plus the scheduling verdict. Removing a
// condition is expressed by its absence from Status.Conditions.
type Decision struct {
	Status       helmv1alpha1.HelmClusterAddonRepositoryStatus
	RequeueAfter time.Duration
	Err          error
}

// abnormalCondition is an optional abnormal-true condition.
type abnormalCondition struct {
	set     bool
	reason  string
	message string
}

// Evaluate derives the desired repository status from the results of a single
// reconcile pass.
func Evaluate(in Inputs) Decision {
	var status helmv1alpha1.HelmClusterAddonRepositoryStatus
	in.Current.DeepCopyInto(&status)
	status.ObservedGeneration = in.Generation

	internal := in.InternalRepository
	if in.InternalRepositoryErr != nil {
		// A failure to reconcile the internal object is a structural failure:
		// report it the same way as an unhealthy object so Ready reflects it.
		internal = services.InternalRepositoryState{
			Present: true,
			Reason:  helmv1alpha1.ReasonFailed,
			Message: in.InternalRepositoryErr.Error(),
		}
	}

	fetchFailed := in.Attempted && in.Fetch != nil && in.Fetch.Err != nil
	fetchSucceeded := in.Attempted && in.Fetch != nil && in.Fetch.Err == nil
	catalogFailed := in.Attempted && in.Catalog != nil && in.Catalog.Err != nil

	failures := in.Current.ConsecutiveFetchFailures
	if in.Generation != in.Current.ObservedGeneration {
		// The spec changed: previous failures were about the previous source.
		failures = 0
	}
	failures = nextFailureCount(failures, in.Attempted, fetchFailed, in.Fetch)

	stalled := evaluateStalled(in, internal, failures, fetchFailed)
	ready := evaluateReady(in, internal, stalled, fetchSucceeded)
	reconciling := evaluateReconciling(in, internal, stalled, ready, failures, catalogFailed)

	setCondition(&status, in, helmv1alpha1.ConditionTypeReady, ready.Status, ready.Reason, ready.Message)

	if in.Attempted {
		syncedStatus, syncedReason, syncedMessage := evaluateSynced(in, fetchFailed, catalogFailed)
		setCondition(&status, in, helmv1alpha1.ConditionTypeSynced, syncedStatus, syncedReason, syncedMessage)
	}

	applyAbnormal(&status, in, helmv1alpha1.ConditionTypeReconciling, reconciling)
	applyAbnormal(&status, in, helmv1alpha1.ConditionTypeStalled, stalled)

	status.ConsecutiveFetchFailures = failures

	if in.Attempted {
		if fetchSucceeded && !catalogFailed && in.Fetch.Pending == 0 {
			status.LastSuccessfulSyncTime = &metav1.Time{Time: in.Now}
		}

		next := in.Now.Add(withJitter(syncDelay(failures), in.Jitter))
		status.NextSyncTime = &metav1.Time{Time: next}
	}

	requeueAfter := time.Duration(0)
	if in.ConfigErr == nil && status.NextSyncTime != nil {
		requeueAfter = status.NextSyncTime.Sub(in.Now)
		if requeueAfter < minRequeue {
			requeueAfter = minRequeue
		}
	}

	return Decision{
		Status:       status,
		RequeueAfter: requeueAfter,
		Err:          firstErr(in.SecretsErr, in.InternalRepositoryErr, catalogErr(in)),
	}
}

func evaluateReady(
	in Inputs,
	internal services.InternalRepositoryState,
	stalled abnormalCondition,
	fetchSucceeded bool,
) metav1.Condition {
	switch {
	case in.SecretsErr != nil:
		return metav1.Condition{
			Status:  metav1.ConditionFalse,
			Reason:  helmv1alpha1.ReasonAuxiliaryResourcesFailed,
			Message: "Failed to reconcile auxiliary resources: " + in.SecretsErr.Error(),
		}
	case internal.Present && !internal.Ready:
		return metav1.Condition{
			Status:  metav1.ConditionFalse,
			Reason:  internal.Reason,
			Message: internal.Message,
		}
	case stalled.set:
		return metav1.Condition{
			Status:  metav1.ConditionFalse,
			Reason:  stalled.reason,
			Message: stalled.message,
		}
	case fetchSucceeded:
		return metav1.Condition{Status: metav1.ConditionTrue, Reason: helmv1alpha1.ReasonSuccess}
	case hasEvidence(in.Current, in.Generation):
		return metav1.Condition{Status: metav1.ConditionTrue, Reason: helmv1alpha1.ReasonSuccess}
	default:
		return metav1.Condition{
			Status:  metav1.ConditionUnknown,
			Reason:  helmv1alpha1.ReasonAwaitingInitialSync,
			Message: "Waiting for the first successful repository read",
		}
	}
}

func evaluateSynced(in Inputs, fetchFailed, catalogFailed bool) (metav1.ConditionStatus, string, string) {
	switch {
	case fetchFailed:
		return metav1.ConditionFalse, helmv1alpha1.ReasonSyncFailed, in.Fetch.Message
	case catalogFailed:
		return metav1.ConditionFalse, helmv1alpha1.ReasonCatalogUpdateFailed,
			"Failed to update the chart catalog: " + in.Catalog.Err.Error()
	case in.Fetch != nil && in.Fetch.Pending > 0 && in.Current.LastSuccessfulSyncTime == nil:
		// On the very first pass there is no other signal that the read was incomplete:
		// lastSuccessfulSyncTime is empty either way, so a user who just created the
		// repository would see Synced=True over a partial catalog. Once a full pass has
		// happened, the frozen Last Sync column carries that signal instead and Synced
		// stops flapping because of a single unreadable tag. The state does not escalate:
		// the failure counter is untouched and Stalled is never reached. The nil guard on
		// in.Fetch protects against a pre-fetch cluster failure, which leaves Fetch nil
		// while still setting Attempted; that case is matched by fetchFailed/catalogFailed
		// above today, but the invariant lives elsewhere and must not be relied on here.
		return metav1.ConditionFalse, helmv1alpha1.ReasonPartialSync,
			fmt.Sprintf("The first repository read left %d chart versions unresolved", in.Fetch.Pending)
	default:
		return metav1.ConditionTrue, helmv1alpha1.ReasonSuccess, ""
	}
}

func evaluateStalled(
	in Inputs,
	internal services.InternalRepositoryState,
	failures int32,
	fetchFailed bool,
) abnormalCondition {
	switch {
	case in.ConfigErr != nil:
		return abnormalCondition{set: true, reason: in.ConfigErr.Reason, message: in.ConfigErr.Message}
	case internal.Present && internal.Stalled:
		return abnormalCondition{set: true, reason: internal.Reason, message: internal.Message}
	case !in.Attempted:
		// A pass without an attempt carries the previous verdict forward so the
		// specific reason is not replaced by a generic one. Only carry it forward
		// when it was set for the current generation: a generation bump voids
		// evidence about the previous spec, mirroring hasEvidence.
		if cond := apimeta.FindStatusCondition(in.Current.Conditions, helmv1alpha1.ConditionTypeStalled); cond != nil &&
			cond.Status == metav1.ConditionTrue && cond.ObservedGeneration == in.Generation {
			return abnormalCondition{set: true, reason: cond.Reason, message: cond.Message}
		}

		return abnormalCondition{}
	case fetchFailed && in.Fetch.Terminal:
		return abnormalCondition{set: true, reason: in.Fetch.Reason, message: in.Fetch.Message}
	case failures >= MaxFetchFailures:
		return abnormalCondition{
			set:     true,
			reason:  helmv1alpha1.ReasonRetriesExceeded,
			message: "Giving up on the repository after repeated read failures",
		}
	default:
		return abnormalCondition{}
	}
}

func evaluateReconciling(
	in Inputs,
	internal services.InternalRepositoryState,
	stalled abnormalCondition,
	ready metav1.Condition,
	failures int32,
	catalogFailed bool,
) abnormalCondition {
	switch {
	case stalled.set:
		return abnormalCondition{}
	case internal.Present && !internal.Ready:
		return abnormalCondition{set: true, reason: helmv1alpha1.ReasonReconciling, message: internal.Message}
	case in.SecretsErr != nil:
		return abnormalCondition{
			set:     true,
			reason:  helmv1alpha1.ReasonProgressingWithRetry,
			message: "Retrying after an auxiliary resource failure",
		}
	case catalogFailed:
		return abnormalCondition{
			set:     true,
			reason:  helmv1alpha1.ReasonProgressingWithRetry,
			message: "Retrying after a chart catalog update failure",
		}
	case !in.Attempted && carriesCatalogFailure(in):
		// The catalog write failure is handed to the work queue, whose immediate
		// retry runs a pass with no attempt, so the case above cannot see it. Carry
		// the retry forward the way evaluateStalled carries its verdict: without it
		// the object would show Ready=True, Synced=False and no abnormal-true
		// condition at all, which kstatus reads as healthy, and the fetch-failure
		// counter — which deliberately ignores catalog failures — would never
		// escalate it to Stalled either.
		return abnormalCondition{
			set:     true,
			reason:  helmv1alpha1.ReasonProgressingWithRetry,
			message: "Retrying after a chart catalog update failure",
		}
	case failures > 0:
		return abnormalCondition{
			set:     true,
			reason:  helmv1alpha1.ReasonProgressingWithRetry,
			message: "Retrying the repository read",
		}
	case ready.Status == metav1.ConditionUnknown:
		return abnormalCondition{
			set:     true,
			reason:  helmv1alpha1.ReasonAwaitingInitialSync,
			message: ready.Message,
		}
	default:
		return abnormalCondition{}
	}
}

// carriesCatalogFailure reports whether the current status still records an
// unresolved chart catalog write failure. Only a record made for the current
// generation counts: a generation bump voids evidence about the previous spec,
// mirroring hasEvidence and the carry-forward in evaluateStalled.
func carriesCatalogFailure(in Inputs) bool {
	cond := apimeta.FindStatusCondition(in.Current.Conditions, helmv1alpha1.ConditionTypeSynced)

	return cond != nil && cond.Status == metav1.ConditionFalse &&
		cond.Reason == helmv1alpha1.ReasonCatalogUpdateFailed &&
		cond.ObservedGeneration == in.Generation
}

// hasEvidence reports whether the repository is already proven usable on the
// current generation: a fetch succeeded for this spec.
func hasEvidence(current helmv1alpha1.HelmClusterAddonRepositoryStatus, generation int64) bool {
	// Evidence is "a fetch succeeded on this spec". Ready alone cannot carry it:
	// a higher-priority rule (an unhealthy internal repository, a failed secret)
	// owns Ready on the very pass where the fetch succeeded, overwriting it.
	// Synced is written only on a pass that attempted, and is True only when the
	// fetch and the catalog write both succeeded, so Synced=True on the current
	// generation means exactly that.
	for _, conditionType := range []string{helmv1alpha1.ConditionTypeReady, helmv1alpha1.ConditionTypeSynced} {
		if cond := apimeta.FindStatusCondition(current.Conditions, conditionType); cond != nil &&
			cond.Status == metav1.ConditionTrue && cond.ObservedGeneration == generation {
			return true
		}
	}

	return false
}

func setCondition(
	status *helmv1alpha1.HelmClusterAddonRepositoryStatus,
	in Inputs,
	conditionType string,
	conditionStatus metav1.ConditionStatus,
	reason, message string,
) {
	apimeta.SetStatusCondition(&status.Conditions, metav1.Condition{
		Type:               conditionType,
		Status:             conditionStatus,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: in.Generation,
		LastTransitionTime: metav1.NewTime(in.Now),
	})
}

func applyAbnormal(
	status *helmv1alpha1.HelmClusterAddonRepositoryStatus,
	in Inputs,
	conditionType string,
	cond abnormalCondition,
) {
	if !cond.set {
		apimeta.RemoveStatusCondition(&status.Conditions, conditionType)

		return
	}

	setCondition(status, in, conditionType, metav1.ConditionTrue, cond.reason, cond.message)
}

func firstErr(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}

	return nil
}

func catalogErr(in Inputs) error {
	if in.Catalog == nil {
		return nil
	}

	return in.Catalog.Err
}

const (
	// SyncInterval is the normal catalog synchronization cadence and the base of
	// the retry backoff, so a broken repository is never polled more often than a
	// healthy one.
	SyncInterval = 5 * time.Minute
	// MaxSyncBackoff caps the retry delay.
	MaxSyncBackoff = 1 * time.Hour
	// MaxFetchFailures is the number of consecutive read failures after which the
	// repository is reported as Stalled. Reaching it does not stop the retries:
	// the cause may disappear on the remote side. Moved here from task 4's
	// standalone declaration — the value does not change.
	MaxFetchFailures = 5
	// SyncBackoffJitter spreads the schedule of repositories that share a remote.
	SyncBackoffJitter = 0.1
	// minRequeue keeps a due schedule from being reported as "no requeue",
	// which is what a zero RequeueAfter means to controller-runtime.
	minRequeue = time.Second
)

// ShouldAttempt reports whether a synchronization attempt is due. The caller
// additionally requires the auxiliary resources to be in place.
func ShouldAttempt(
	current helmv1alpha1.HelmClusterAddonRepositoryStatus,
	generation int64,
	now time.Time,
	forced bool,
) bool {
	switch {
	case forced:
		return true
	case generation != current.ObservedGeneration:
		return true
	case current.NextSyncTime == nil:
		return true
	default:
		return !now.Before(current.NextSyncTime.Time)
	}
}

// NewJitter returns the random factor Evaluate applies to the computed delay.
// It lives outside Evaluate to keep that function deterministic.
func NewJitter() float64 {
	return (rand.Float64()*2 - 1) * SyncBackoffJitter
}

func syncDelay(failures int32) time.Duration {
	if failures <= 0 {
		return SyncInterval
	}

	delay := SyncInterval << (failures - 1)
	if delay > MaxSyncBackoff || delay <= 0 {
		return MaxSyncBackoff
	}

	return delay
}

func withJitter(delay time.Duration, jitter float64) time.Duration {
	if jitter == 0 {
		return delay
	}

	return delay + time.Duration(float64(delay)*jitter)
}

func nextFailureCount(failures int32, attempted, fetchFailed bool, fetch *services.FetchOutcome) int32 {
	if !attempted {
		return failures
	}

	if fetch == nil {
		// The pass ran but never reached the registry (a cluster-side failure, such
		// as knownCharts, happened first). That is not evidence the registry
		// recovered, so the counter is carried forward rather than reset.
		return failures
	}

	if !fetchFailed {
		return 0
	}

	if fetch.Terminal {
		// A terminal failure is reported immediately; saturating the counter keeps
		// a single formula for the schedule and puts the retry at the cap.
		return MaxFetchFailures
	}

	if failures < MaxFetchFailures {
		return failures + 1
	}

	return failures
}
