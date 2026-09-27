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

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
	"github.com/deckhouse/operator-helm/internal/internalobj"
	"github.com/deckhouse/operator-helm/internal/reposync"
)

// Inputs carries everything Evaluate needs. It holds no clients and no clock:
// Now and Jitter are supplied by the caller so the function is deterministic
// and unit testable.
type Inputs struct {
	Generation int64
	Now        time.Time
	Jitter     float64
	Current    helmv1alpha1.RepositoryStatus

	SecretsErr            error
	InternalRepositoryErr error
	InternalRepository    internalobj.InternalRepositoryState
	ConfigErr             *ConfigFailure

	// MigrateErr reports a failure to move the catalog objects to their current
	// names. It is not tied to a synchronization attempt: the rename runs on every
	// pass, whether or not the remote was read.
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
	// Confirmed on every pass, unlike the release package's observedGeneration, which
	// is held back until the pass settles. A repository has a second field for the
	// verdict: lastSyncedGeneration, written only by a pass that published a catalog,
	// is what says which spec the catalog in front of a reader belongs to, and it is
	// the one releases read (catalogIsCurrent in internal/reconcile/release). That
	// leaves observedGeneration to record which spec this pass ran against, and the
	// failure counter below is its only reader: the count resets on the pass that
	// first sees a new generation. Holding it back would make every pass look like
	// that first one and reset the count again and again, so consecutive failures
	// would never add up to the backoff and the stall they are counted for.
	status.ObservedGeneration = in.Generation

	internal := in.InternalRepository
	if in.InternalRepositoryErr != nil {
		// A failure to reconcile the internal object is a structural failure:
		// report it the same way as an unhealthy object so Ready reflects it.
		internal = internalobj.InternalRepositoryState{
			Present: true,
			Reason:  helmv1alpha1.ReasonFailed,
			Message: in.InternalRepositoryErr.Error(),
		}
	}

	fetchFailed := in.Attempted && in.Fetch != nil && in.Fetch.Err != nil
	fetchSucceeded := in.Attempted && in.Fetch != nil && in.Fetch.Err == nil
	catalogFailed := in.MigrateErr != nil || (in.Attempted && in.Catalog != nil && in.Catalog.Err != nil)

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

	// A rename failure is reported even on a pass that attempted no synchronization:
	// the rename runs on every pass by design, so gating its verdict on an attempt
	// would leave the previous Synced=True standing while consumers cannot resolve.
	if in.Attempted || in.MigrateErr != nil {
		syncedStatus, syncedReason, syncedMessage := evaluateSynced(in, fetchFailed, catalogFailed)
		setCondition(&status, in, helmv1alpha1.ConditionTypeSynced, syncedStatus, syncedReason, syncedMessage)
	}

	applyAbnormal(&status, in, helmv1alpha1.ConditionTypeReconciling, reconciling)
	applyAbnormal(&status, in, helmv1alpha1.ConditionTypeStalled, stalled)

	status.ConsecutiveFetchFailures = failures

	if in.Attempted {
		if in.Forced {
			// Stamped on the attempt, because the attempt is what consumes the force
			// annotation. The stamp records that the request was acted on, not that it
			// succeeded — the outcome is carried by Ready and Synced.
			status.LastForceReconcileTime = &metav1.Time{Time: in.Now}
		}

		if fetchSucceeded {
			// Written on every successful read, not only on a complete pass: it
			// describes what the repository offers, and a pass left incomplete by a
			// pending tag still read the full list of charts.
			//
			// The charts the catalog could not publish are subtracted, because this
			// number is what a user compares against the chart objects in the cluster:
			// counting a chart that has no object would make the comparison come out
			// wrong with nothing in the status to explain it. Synced names them
			// instead. A catalog write that failed part way reports only the charts it
			// reached, so the count can be too high until the retry succeeds — which
			// the failing Synced condition already says. The catalog can fail on its
			// very first chart, so "too high" reaches as far as the whole index.
			status.ChartCount = ptr.To(int32(in.Fetch.Charts - len(skippedCharts(in))))
		}

		if fetchSucceeded && !catalogFailed {
			// Written on every pass that published a catalog for this spec, pending tags
			// included: a version the registry has not answered about is listed like any
			// other, so the catalog is still the repository's full answer as to which
			// versions exist — which is the only question releases ask it. Holding the
			// generation back until nothing is pending would never release it at all on a
			// repository with one permanently unreadable tag.
			//
			// The condition cannot carry this: Synced is the verdict of the latest pass,
			// so a transient fetch failure turns it False over a catalog that is complete
			// and unchanged. Neither can lastSuccessfulSyncTime, which says when the
			// repository was last mirrored in full but not which spec that was.
			status.LastSyncedGeneration = in.Generation

			if in.Fetch.Pending == 0 {
				status.LastSuccessfulSyncTime = &metav1.Time{Time: in.Now}
			}
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
		Err:          firstErr(in.SecretsErr, in.InternalRepositoryErr, in.MigrateErr, catalogErr(in)),
	}
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

		return metav1.ConditionFalse, helmv1alpha1.ReasonCatalogUpdateFailed,
			"Failed to update the chart catalog: " + err.Error()
	default:
		if message := partialSyncMessage(in); message != "" {
			return metav1.ConditionFalse, helmv1alpha1.ReasonPartialSync, message
		}

		return metav1.ConditionTrue, helmv1alpha1.ReasonSuccess, ""
	}
}

// partialSyncMessage describes every way the catalog of a pass that read the
// repository and wrote what it could still does not mirror it. An empty string
// means it does mirror it.
func partialSyncMessage(in Inputs) string {
	var parts []string

	if skipped := skippedCharts(in); len(skipped) > 0 {
		// Reported on every pass, not only the first one: a name the API cannot hold
		// does not become valid on its own, so the chart stays out of the catalog for
		// as long as the repository offers it, and the status is the only place a user
		// can learn why chartCount is lower than the repository's own index. Repeating
		// the same reason and message writes nothing — the condition, including its
		// transition time, stays as it is — so saying it every pass is not noise. That
		// holds only because describeCharts sorts: the same set of names has to spell
		// the same message, whatever order the pass collected them in.
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
		// On the very first pass there is no other signal that the read was incomplete:
		// lastSuccessfulSyncTime is empty either way, so a user who just created the
		// repository would see Synced=True over a partial catalog. Once a full pass has
		// happened, the frozen Last Sync column carries that signal instead and Synced
		// stops flapping because of a single unreadable tag. The state does not escalate:
		// the failure counter is untouched and Stalled is never reached. The nil guard on
		// in.Fetch protects against a pre-fetch cluster failure, which leaves Fetch nil
		// while still setting Attempted; that case is matched by fetchFailed/catalogFailed
		// in evaluateSynced today, but the invariant lives elsewhere and must not be
		// relied on here.
		parts = append(parts, fmt.Sprintf("The first repository read left %d chart versions unresolved", in.Fetch.Pending))
	}

	return strings.Join(parts, ". ")
}

// skippedCharts names the charts this pass could not publish. Catalog is nil on a
// pass that attempted nothing, and on one whose fetch never returned.
func skippedCharts(in Inputs) []string {
	if in.Catalog == nil {
		return nil
	}

	return in.Catalog.SkippedCharts
}

// describeCharts lists chart names for a condition message.
//
// The names are sorted, and on a copy so the outcome the caller holds is left
// alone. Order is not a property of the input: a Helm index is a map on the wire,
// so the same repository yields its charts in a different order every read. Without
// sorting, the same set of skipped charts would produce a different message on each
// pass, every pass would rewrite the condition, and the repository would be patched
// forever over a state that never changed. The transition time stays put either way
// — it follows the status, not the message — so it is the write itself that has to
// be avoided.
//
// Only a few names are shown and each is cut short. A message is limited to 32768
// bytes by the API server, and an index whose naming convention the API rejects
// wholesale — with names as long as an index may legally publish — would otherwise
// make the status unwritable and cost the repository its every other verdict too.
// Both bounds are needed: a count alone does not bound the length.
//
// The names are quoted because a name that got here is not a label value and may
// well be empty or hold spaces.
func describeCharts(names []string) string {
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

// truncateChartName cuts one name down to the part worth showing. It counts runes,
// not bytes, so the cut never lands inside a character. Quoting can still multiply
// what is left — an unprintable byte becomes an escape of up to ten bytes — which is
// why the bound is stated in the small: maxNamedCharts names of at most
// maxChartNameRunes runes cannot approach the 32768 bytes a message may hold, even
// when every rune has to be escaped.
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
	// maxChartNameRunes bounds how much of one name it shows. A chart name this long
	// is already past anything a user reads at a glance, and the point of the message
	// is to identify the chart in the index, not to reproduce it.
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
func hasEvidence(current helmv1alpha1.RepositoryStatus, generation int64) bool {
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

func nextFailureCount(failures int32, attempted, fetchFailed bool, fetch *reposync.FetchOutcome) int32 {
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
