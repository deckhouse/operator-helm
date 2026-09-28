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

package repository

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"strconv"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
	"github.com/deckhouse/operator-helm/internal/internalobj"
	"github.com/deckhouse/operator-helm/internal/reposync"
)

// Inputs carries everything Evaluate needs. It holds no clients and no clock: Now and
// Jitter are supplied by the caller.
type Inputs struct {
	Generation int64
	Now        time.Time
	Jitter     float64
	Current    helmv1alpha1.RepositoryStatus

	SecretsErr            error
	InternalRepositoryErr error
	InternalRepository    internalobj.InternalRepositoryState
	ConfigErr             *ConfigFailure

	// MigrateErr reports a failure to move the catalog objects to their current names. It is
	// not tied to a synchronization attempt.
	//
	// TRANSITIONAL: remove together with the catalog's own migration.
	MigrateErr error

	// Forced reports whether this pass was requested through the force reconcile
	// annotation.
	Forced bool

	// Attempted reports whether a synchronization attempt ran in this pass.
	// Fetch and Catalog are nil when it did not.
	Attempted bool
	Fetch     *reposync.FetchOutcome
	Catalog   *reposync.CatalogOutcome

	// SyncEnd is when the synchronization attempt finished, and is zero on a pass that
	// attempted none. Only the schedule uses it; every recorded timestamp stays on Now.
	SyncEnd time.Time
}

// Decision is the full desired status plus the scheduling verdict. Removing a
// condition is expressed by its absence from Status.Conditions.
type Decision struct {
	Status       helmv1alpha1.RepositoryStatus
	RequeueAfter time.Duration
	Err          error
}

type abnormalCondition struct {
	set     bool
	reason  string
	message string
}

// Evaluate derives the desired repository status from the results of a single
// reconcile pass.
func Evaluate(in Inputs) Decision {
	var status helmv1alpha1.RepositoryStatus
	in.Current.DeepCopyInto(&status)
	// Confirmed on every pass, not held back: lastSyncedGeneration carries the verdict instead.
	status.ObservedGeneration = in.Generation

	internal := in.InternalRepository
	if in.InternalRepositoryErr != nil {
		// Reported the same way as an unhealthy object, so Ready reflects it.
		internal = internalobj.InternalRepositoryState{
			Present: true,
			Reason:  helmv1alpha1.ReasonFailed,
			Message: in.InternalRepositoryErr.Error(),
		}
	}

	fetchFailed := in.Attempted && in.Fetch != nil && in.Fetch.Err != nil
	fetchSucceeded := in.Attempted && in.Fetch != nil && in.Fetch.Err == nil
	fetchIncomplete := fetchSucceeded && in.Fetch.Unexamined > 0
	catalogFailed := in.MigrateErr != nil || (in.Attempted && in.Catalog != nil && in.Catalog.Err != nil)
	// A lost race rather than a failure; a fetch failure cannot have happened alongside it.
	catalogRaced := catalogFailed && apierrors.IsConflict(firstErr(in.MigrateErr, catalogErr(in)))

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

	// A rename failure is reported even on a pass that attempted no synchronization.
	if in.Attempted || in.MigrateErr != nil {
		syncedStatus, syncedReason, syncedMessage := evaluateSynced(in, fetchFailed, catalogFailed)
		setCondition(&status, in, helmv1alpha1.ConditionTypeSynced, syncedStatus, syncedReason, syncedMessage)
	}

	applyAbnormal(&status, in, helmv1alpha1.ConditionTypeReconciling, reconciling)
	applyAbnormal(&status, in, helmv1alpha1.ConditionTypeStalled, stalled)

	status.ConsecutiveFetchFailures = failures

	if in.Attempted {
		if in.Forced {
			// Stamped on the attempt: it records that the request was acted on, not that it worked.
			status.LastForceReconcileTime = &metav1.Time{Time: in.Now}
		}

		if fetchSucceeded {
			// Skipped charts are subtracted, so the count matches the chart objects in the cluster.
			status.ChartCount = ptr.To(int32(in.Fetch.Charts - len(skippedCharts(in))))
		}

		if fetchSucceeded && !catalogFailed {
			// Written in both directions by a pass that published a catalog, left alone by one
			// that did not: it describes the catalog a reader has in front of it.
			status.UnexaminedVersions = int32(in.Fetch.Unexamined)
		}

		if fetchSucceeded && !catalogFailed && !fetchIncomplete {
			// Written with pending tags included, but never by a pass that ran out of its budget:
			// only then is the catalog the repository's full answer as to which versions exist.
			status.LastSyncedGeneration = in.Generation

			if in.Fetch.Pending == 0 {
				status.LastSuccessfulSyncTime = &metav1.Time{Time: in.Now}
			}
		}

		delay := syncDelay(failures)
		if (fetchIncomplete || catalogRaced) && delay > IncompleteSyncDelay {
			// The next pass picks up where this one stopped, but still waits: it held a worker.
			delay = IncompleteSyncDelay
		}

		next := scheduleBase(in).Add(withJitter(delay, in.Jitter))
		status.NextSyncTime = &metav1.Time{Time: next}
	}

	requeueAfter := time.Duration(0)
	if in.ConfigErr == nil && status.NextSyncTime != nil {
		requeueAfter = status.NextSyncTime.Sub(scheduleBase(in))
		if requeueAfter < minRequeue {
			requeueAfter = minRequeue
		}
	}

	return Decision{
		Status:       status,
		RequeueAfter: requeueAfter,
		Err:          firstErr(in.SecretsErr, in.InternalRepositoryErr, in.MigrateErr, catalogErr(in)),
	}
}

// scheduleBase is the instant the wait until the next attempt is measured from: the end of
// the attempt this pass made, or Now when it made none.
func scheduleBase(in Inputs) time.Time {
	if in.Attempted && !in.SyncEnd.IsZero() {
		return in.SyncEnd
	}

	return in.Now
}

func evaluateReady(
	in Inputs,
	internal internalobj.InternalRepositoryState,
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
		err := in.MigrateErr
		if err == nil {
			err = in.Catalog.Err
		}

		if apierrors.IsConflict(err) {
			// Named rather than reported verbatim: the API's conflict wording reads as a defect.
			return metav1.ConditionFalse, helmv1alpha1.ReasonCatalogUpdateFailed,
				"The chart catalog changed since this pass read it; retrying"
		}

		return metav1.ConditionFalse, helmv1alpha1.ReasonCatalogUpdateFailed,
			"Failed to update the chart catalog: " + err.Error()
	default:
		if message := partialSyncMessage(in); message != "" {
			return metav1.ConditionFalse, helmv1alpha1.ReasonPartialSync, message
		}

		return metav1.ConditionTrue, helmv1alpha1.ReasonSuccess, ""
	}
}

// partialSyncMessage describes every way the catalog of a pass that read the repository and
// wrote what it could still does not mirror it. An empty string means it does mirror it.
func partialSyncMessage(in Inputs) string {
	var parts []string

	if skipped := skippedCharts(in); len(skipped) > 0 {
		// Reported on every pass; repeating an identical reason and message writes nothing.
		noun := "charts"
		if len(skipped) == 1 {
			noun = "chart"
		}

		parts = append(parts, fmt.Sprintf(
			"The catalog leaves out %d %s whose name cannot be recorded on a catalog object: %s",
			len(skipped), noun, describeCharts(skipped),
		))
	}

	if in.Fetch != nil && in.Fetch.Pending > 0 && in.Current.LastSuccessfulSyncTime == nil {
		// Only on the first pass: afterwards the frozen Last Sync time carries the same signal.
		parts = append(parts, fmt.Sprintf("The first repository read left %d chart versions unresolved", in.Fetch.Pending))
	}

	if in.Fetch != nil && in.Fetch.Unexamined > 0 {
		// Said on every such pass: the count is what shows the next pass making progress.
		parts = append(parts, fmt.Sprintf(
			"The repository read ran out of time with %d chart versions still unexamined; another pass is scheduled",
			in.Fetch.Unexamined,
		))
	}

	return strings.Join(parts, ". ")
}

// skippedCharts names the charts this pass could not publish. Catalog is nil on a pass that
// attempted nothing, and on one whose fetch never returned.
func skippedCharts(in Inputs) []string {
	if in.Catalog == nil {
		return nil
	}

	return in.Catalog.SkippedCharts
}

// describeCharts lists chart names for a condition message, sorted on a copy, quoted, bounded
// in count and in length. A message may hold at most 32768 bytes.
func describeCharts(names []string) string {
	// A Helm index is a map on the wire, so an unsorted message would differ on every pass.
	shown := slices.Clone(names)
	slices.Sort(shown)

	suffix := ""
	if len(shown) > maxNamedCharts {
		suffix = fmt.Sprintf(" and %d more", len(shown)-maxNamedCharts)
		shown = shown[:maxNamedCharts]
	}

	quoted := make([]string, 0, len(shown))
	for _, name := range shown {
		quoted = append(quoted, strconv.Quote(truncateChartName(name)))
	}

	return strings.Join(quoted, ", ") + suffix
}

// truncateChartName cuts one name down to the part worth showing, counting runes rather than
// bytes so the cut never lands inside a character.
func truncateChartName(name string) string {
	runes := []rune(name)
	if len(runes) <= maxChartNameRunes {
		return name
	}

	return string(runes[:maxChartNameRunes]) + "..."
}

const (
	// maxNamedCharts bounds how many names describeCharts shows.
	maxNamedCharts = 3
	// maxChartNameRunes bounds how much of one name it shows.
	maxChartNameRunes = 128
)

func evaluateStalled(
	in Inputs,
	internal internalobj.InternalRepositoryState,
	failures int32,
	fetchFailed bool,
) abnormalCondition {
	switch {
	case in.ConfigErr != nil:
		return abnormalCondition{set: true, reason: in.ConfigErr.Reason, message: in.ConfigErr.Message}
	case internal.Present && internal.Stalled:
		return abnormalCondition{set: true, reason: internal.Reason, message: internal.Message}
	case !in.Attempted:
		// Carried forward only for the current generation: a bump voids the previous spec's verdict.
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
	internal internalobj.InternalRepositoryState,
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
		// The work queue's immediate retry runs a pass with no attempt, which the case above misses.
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

// carriesCatalogFailure reports whether the current status still records an unresolved chart
// catalog write failure for the current generation.
func carriesCatalogFailure(in Inputs) bool {
	cond := apimeta.FindStatusCondition(in.Current.Conditions, helmv1alpha1.ConditionTypeSynced)

	return cond != nil && cond.Status == metav1.ConditionFalse &&
		cond.Reason == helmv1alpha1.ReasonCatalogUpdateFailed &&
		cond.ObservedGeneration == in.Generation
}

// hasEvidence reports whether the repository is already proven usable on the current
// generation: a fetch succeeded for this spec.
func hasEvidence(current helmv1alpha1.RepositoryStatus, generation int64) bool {
	// Ready alone cannot carry it: a higher-priority rule can own Ready on that very pass.
	for _, conditionType := range []string{helmv1alpha1.ConditionTypeReady, helmv1alpha1.ConditionTypeSynced} {
		if cond := apimeta.FindStatusCondition(current.Conditions, conditionType); cond != nil &&
			cond.Status == metav1.ConditionTrue && cond.ObservedGeneration == generation {
			return true
		}
	}

	return false
}

func setCondition(
	status *helmv1alpha1.RepositoryStatus,
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
	status *helmv1alpha1.RepositoryStatus,
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
	// SyncInterval is the normal catalog synchronization cadence and the base of the retry
	// backoff, so a broken repository is never polled more often than a healthy one.
	SyncInterval = 5 * time.Minute
	// MaxSyncBackoff caps the retry delay.
	MaxSyncBackoff = 1 * time.Hour
	// MaxFetchFailures is the number of consecutive read failures after which the repository
	// is reported as Stalled. Reaching it does not stop the retries.
	MaxFetchFailures = 5
	// IncompleteSyncDelay is the wait after a pass that read the repository but ran out of its
	// budget before examining every version it listed.
	IncompleteSyncDelay = 1 * time.Minute
	// SyncBackoffJitter spreads the schedule of repositories that share a remote.
	SyncBackoffJitter = 0.1
	// minRequeue keeps a due schedule from being reported as "no requeue", which is what a
	// zero RequeueAfter means to controller-runtime.
	minRequeue = time.Second
)

// ShouldAttempt reports whether a synchronization attempt is due. The caller additionally
// requires the auxiliary resources to be in place.
func ShouldAttempt(
	current helmv1alpha1.RepositoryStatus,
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

// NewJitter returns the random factor Evaluate applies to the computed delay. It lives
// outside Evaluate to keep that function deterministic.
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

func nextFailureCount(failures int32, attempted, fetchFailed bool, fetch *reposync.FetchOutcome) int32 {
	if !attempted {
		return failures
	}

	if fetch == nil {
		// The pass never reached the registry, which is no evidence that the registry recovered.
		return failures
	}

	if !fetchFailed {
		return 0
	}

	if fetch.Terminal {
		// Saturating the counter keeps one formula for the schedule and puts the retry at the cap.
		return MaxFetchFailures
	}

	if failures < MaxFetchFailures {
		return failures + 1
	}

	return failures
}
