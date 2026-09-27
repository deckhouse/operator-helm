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
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
	"github.com/deckhouse/operator-helm/internal/internalobj"
	"github.com/deckhouse/operator-helm/internal/reposync"
)

var testNow = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

func readyStatus(generation int64) helmv1alpha1.RepositoryStatus {
	return helmv1alpha1.RepositoryStatus{
		ObservedGeneration: generation,
		Conditions: []metav1.Condition{
			{
				Type:               helmv1alpha1.ConditionTypeReady,
				Status:             metav1.ConditionTrue,
				Reason:             helmv1alpha1.ReasonSuccess,
				ObservedGeneration: generation,
				LastTransitionTime: metav1.NewTime(testNow.Add(-time.Hour)),
			},
			{
				Type:               helmv1alpha1.ConditionTypeSynced,
				Status:             metav1.ConditionTrue,
				Reason:             helmv1alpha1.ReasonSuccess,
				ObservedGeneration: generation,
				LastTransitionTime: metav1.NewTime(testNow.Add(-time.Hour)),
			},
		},
	}
}

// stalledStatus builds a status like readyStatus, plus a Stalled=True condition
// recorded for staleGeneration — used to test that a generation bump voids a
// carried-forward Stalled reason that described the previous spec.
func stalledStatus(generation, staleGeneration int64, reason string) helmv1alpha1.RepositoryStatus {
	status := readyStatus(generation)
	status.Conditions = append(status.Conditions, metav1.Condition{
		Type:               helmv1alpha1.ConditionTypeStalled,
		Status:             metav1.ConditionTrue,
		Reason:             reason,
		ObservedGeneration: staleGeneration,
		LastTransitionTime: metav1.NewTime(testNow.Add(-time.Hour)),
	})

	return status
}

// syncedNotReadyStatus builds the status left behind by the pass on which the
// first fetch succeeded while the internal repository was still unhealthy:
// Synced records the successful read, but Ready was written False by the
// higher-priority internal-repository rule, so Ready alone carries no evidence.
func syncedNotReadyStatus(generation int64) helmv1alpha1.RepositoryStatus {
	return helmv1alpha1.RepositoryStatus{
		ObservedGeneration: generation,
		Conditions: []metav1.Condition{
			{
				Type:               helmv1alpha1.ConditionTypeReady,
				Status:             metav1.ConditionFalse,
				Reason:             "FetchFailed",
				Message:            "failed to fetch index",
				ObservedGeneration: generation,
				LastTransitionTime: metav1.NewTime(testNow.Add(-time.Minute)),
			},
			{
				Type:               helmv1alpha1.ConditionTypeSynced,
				Status:             metav1.ConditionTrue,
				Reason:             helmv1alpha1.ReasonSuccess,
				ObservedGeneration: generation,
				LastTransitionTime: metav1.NewTime(testNow.Add(-time.Minute)),
			},
			{
				Type:               helmv1alpha1.ConditionTypeReconciling,
				Status:             metav1.ConditionTrue,
				Reason:             helmv1alpha1.ReasonReconciling,
				Message:            "failed to fetch index",
				ObservedGeneration: generation,
				LastTransitionTime: metav1.NewTime(testNow.Add(-time.Minute)),
			},
		},
	}
}

// catalogFailedStatus builds the status left behind by a pass whose fetch
// succeeded and whose catalog write failed: Ready stays latched True, Synced is
// False with CatalogUpdateFailed and Reconciling carries the retry.
func catalogFailedStatus(generation int64) helmv1alpha1.RepositoryStatus {
	status := readyStatus(generation)
	apimeta.SetStatusCondition(&status.Conditions, metav1.Condition{
		Type:               helmv1alpha1.ConditionTypeSynced,
		Status:             metav1.ConditionFalse,
		Reason:             helmv1alpha1.ReasonCatalogUpdateFailed,
		Message:            "Failed to update the chart catalog: etcdserver: request timed out",
		ObservedGeneration: generation,
		LastTransitionTime: metav1.NewTime(testNow.Add(-time.Minute)),
	})
	apimeta.SetStatusCondition(&status.Conditions, metav1.Condition{
		Type:               helmv1alpha1.ConditionTypeReconciling,
		Status:             metav1.ConditionTrue,
		Reason:             helmv1alpha1.ReasonProgressingWithRetry,
		Message:            "Retrying after a chart catalog update failure",
		ObservedGeneration: generation,
		LastTransitionTime: metav1.NewTime(testNow.Add(-time.Minute)),
	})

	return status
}

func conditionOf(t *testing.T, status helmv1alpha1.RepositoryStatus, conditionType string) *metav1.Condition {
	t.Helper()

	return apimeta.FindStatusCondition(status.Conditions, conditionType)
}

func TestEvaluateConditions(t *testing.T) {
	fetchErr := errors.New("connection refused")
	writeErr := errors.New("etcdserver: request timed out")

	cases := []struct {
		name string
		in   Inputs

		wantReady       metav1.ConditionStatus
		wantReadyReason string
		wantSynced      metav1.ConditionStatus
		wantReconciling string // reason; "" means the condition must be absent
		wantStalled     string // reason; "" means the condition must be absent
	}{
		{
			name: "healthy repository",
			in: Inputs{
				Generation: 1, Now: testNow, Current: readyStatus(1),
				InternalRepository: internalobj.InternalRepositoryState{Present: true, Ready: true},
				Attempted:          true,
				Fetch:              &reposync.FetchOutcome{},
				Catalog:            &reposync.CatalogOutcome{},
			},
			wantReady: metav1.ConditionTrue, wantReadyReason: helmv1alpha1.ReasonSuccess,
			wantSynced: metav1.ConditionTrue,
		},
		{
			name: "awaiting first sync on a fresh object",
			in: Inputs{
				Generation: 1, Now: testNow,
				InternalRepository: internalobj.InternalRepositoryState{Present: true, Ready: true},
			},
			wantReady: metav1.ConditionUnknown, wantReadyReason: helmv1alpha1.ReasonAwaitingInitialSync,
			wantSynced:      "",
			wantReconciling: helmv1alpha1.ReasonAwaitingInitialSync,
		},
		{
			name: "auxiliary resources failed",
			in: Inputs{
				Generation: 1, Now: testNow, Current: readyStatus(1),
				SecretsErr: writeErr,
			},
			wantReady: metav1.ConditionFalse, wantReadyReason: helmv1alpha1.ReasonAuxiliaryResourcesFailed,
			wantSynced:      metav1.ConditionTrue,
			wantReconciling: helmv1alpha1.ReasonProgressingWithRetry,
		},
		{
			name: "internal repository not ready",
			in: Inputs{
				Generation: 1, Now: testNow, Current: readyStatus(1),
				InternalRepository: internalobj.InternalRepositoryState{
					Present: true, Ready: false,
					Reason: "FetchFailed", Message: "failed to fetch index",
				},
				Attempted: true,
				Fetch:     &reposync.FetchOutcome{},
				Catalog:   &reposync.CatalogOutcome{},
			},
			wantReady: metav1.ConditionFalse, wantReadyReason: "FetchFailed",
			wantSynced:      metav1.ConditionTrue,
			wantReconciling: helmv1alpha1.ReasonReconciling,
		},
		{
			name: "internal repository stalled",
			in: Inputs{
				Generation: 1, Now: testNow, Current: readyStatus(1),
				InternalRepository: internalobj.InternalRepositoryState{
					Present: true, Ready: false, Stalled: true,
					Reason: "InvalidSecretRef", Message: "secret not found",
				},
				Attempted: true,
				Fetch:     &reposync.FetchOutcome{},
				Catalog:   &reposync.CatalogOutcome{},
			},
			wantReady: metav1.ConditionFalse, wantReadyReason: "InvalidSecretRef",
			wantSynced:  metav1.ConditionTrue,
			wantStalled: "InvalidSecretRef",
		},
		{
			name: "transient fetch failure keeps Ready latched",
			in: Inputs{
				Generation: 1, Now: testNow, Current: readyStatus(1),
				InternalRepository: internalobj.InternalRepositoryState{Present: true, Ready: true},
				Attempted:          true,
				Fetch:              &reposync.FetchOutcome{Err: fetchErr, Message: "cannot read index.yaml"},
			},
			wantReady: metav1.ConditionTrue, wantReadyReason: helmv1alpha1.ReasonSuccess,
			wantSynced:      metav1.ConditionFalse,
			wantReconciling: helmv1alpha1.ReasonProgressingWithRetry,
		},
		{
			name: "transient fetch failure without evidence",
			in: Inputs{
				Generation: 1, Now: testNow,
				InternalRepository: internalobj.InternalRepositoryState{Present: true, Ready: true},
				Attempted:          true,
				Fetch:              &reposync.FetchOutcome{Err: fetchErr, Message: "cannot read index.yaml"},
			},
			wantReady: metav1.ConditionUnknown, wantReadyReason: helmv1alpha1.ReasonAwaitingInitialSync,
			wantSynced:      metav1.ConditionFalse,
			wantReconciling: helmv1alpha1.ReasonProgressingWithRetry,
		},
		{
			name: "terminal fetch failure",
			in: Inputs{
				Generation: 1, Now: testNow, Current: readyStatus(1),
				InternalRepository: internalobj.InternalRepositoryState{Present: true, Ready: true},
				Attempted:          true,
				Fetch: &reposync.FetchOutcome{
					Err: fetchErr, Terminal: true,
					Reason:  helmv1alpha1.ReasonAuthenticationFailed,
					Message: "repository rejected the credentials (HTTP 401)",
				},
			},
			wantReady: metav1.ConditionFalse, wantReadyReason: helmv1alpha1.ReasonAuthenticationFailed,
			wantSynced:  metav1.ConditionFalse,
			wantStalled: helmv1alpha1.ReasonAuthenticationFailed,
		},
		{
			// An unparsable spec.caCertificate travels as a terminal fetch outcome, so
			// this is where "not retried" is observable: the repository is Stalled on
			// the reason that names the field, instead of collecting SyncFailed passes
			// against bytes no attempt can parse.
			name: "invalid ca certificate",
			in: Inputs{
				Generation: 1, Now: testNow, Current: readyStatus(1),
				InternalRepository: internalobj.InternalRepositoryState{Present: true, Ready: true},
				Attempted:          true,
				Fetch: &reposync.FetchOutcome{
					Err: fetchErr, Terminal: true,
					Reason:  helmv1alpha1.ReasonInvalidCACertificate,
					Message: "spec.caCertificate holds no PEM encoded certificate",
				},
			},
			wantReady: metav1.ConditionFalse, wantReadyReason: helmv1alpha1.ReasonInvalidCACertificate,
			wantSynced:  metav1.ConditionFalse,
			wantStalled: helmv1alpha1.ReasonInvalidCACertificate,
		},
		{
			name: "unsupported repository type",
			in: Inputs{
				Generation: 1, Now: testNow, Current: readyStatus(1),
				ConfigErr: &ConfigFailure{
					Reason:  helmv1alpha1.ReasonUnsupportedRepositoryType,
					Message: "unsupported repository schema in use: ftp",
				},
			},
			wantReady: metav1.ConditionFalse, wantReadyReason: helmv1alpha1.ReasonUnsupportedRepositoryType,
			wantSynced:  metav1.ConditionTrue,
			wantStalled: helmv1alpha1.ReasonUnsupportedRepositoryType,
		},
		{
			name: "catalog write failure",
			in: Inputs{
				Generation: 1, Now: testNow, Current: readyStatus(1),
				InternalRepository: internalobj.InternalRepositoryState{Present: true, Ready: true},
				Attempted:          true,
				Fetch:              &reposync.FetchOutcome{},
				Catalog:            &reposync.CatalogOutcome{Err: writeErr},
			},
			wantReady: metav1.ConditionTrue, wantReadyReason: helmv1alpha1.ReasonSuccess,
			wantSynced:      metav1.ConditionFalse,
			wantReconciling: helmv1alpha1.ReasonProgressingWithRetry,
		},
		{
			name: "generation bump voids the latch",
			in: Inputs{
				Generation: 2, Now: testNow, Current: readyStatus(1),
				InternalRepository: internalobj.InternalRepositoryState{Present: true, Ready: true},
				Attempted:          true,
				Fetch:              &reposync.FetchOutcome{Err: fetchErr, Message: "cannot read index.yaml"},
			},
			wantReady: metav1.ConditionUnknown, wantReadyReason: helmv1alpha1.ReasonAwaitingInitialSync,
			wantSynced:      metav1.ConditionFalse,
			wantReconciling: helmv1alpha1.ReasonProgressingWithRetry,
		},
		{
			name: "oci repository has no internal object",
			in: Inputs{
				Generation: 1, Now: testNow, Current: readyStatus(1),
				InternalRepository: internalobj.InternalRepositoryState{Present: false},
				Attempted:          true,
				Fetch:              &reposync.FetchOutcome{},
				Catalog:            &reposync.CatalogOutcome{},
			},
			wantReady: metav1.ConditionTrue, wantReadyReason: helmv1alpha1.ReasonSuccess,
			wantSynced: metav1.ConditionTrue,
		},
		{
			// The fetch succeeded on an earlier pass, but the internal repository
			// was unhealthy then, so the higher-priority rule owned Ready and wrote
			// it False. Ready alone therefore carries no evidence; Synced=True on
			// this generation does, and must keep the repository from falling back
			// to Unknown/AwaitingInitialSync until the next scheduled sync.
			name: "synced carries the evidence when Ready was owned by another rule",
			in: Inputs{
				Generation: 1, Now: testNow, Current: syncedNotReadyStatus(1),
				InternalRepository: internalobj.InternalRepositoryState{Present: true, Ready: true},
			},
			wantReady: metav1.ConditionTrue, wantReadyReason: helmv1alpha1.ReasonSuccess,
			wantSynced: metav1.ConditionTrue,
		},
		{
			// The work-queue retry after a catalog write failure runs a pass with no
			// attempt. Without a carry-forward the repository would show Ready=True,
			// Synced=False and no abnormal-true condition at all — Current to
			// kstatus — and ConsecutiveFetchFailures never escalates it to Stalled.
			name: "catalog write failure keeps Reconciling on a pass with no attempt",
			in: Inputs{
				Generation: 1, Now: testNow, Current: catalogFailedStatus(1),
				InternalRepository: internalobj.InternalRepositoryState{Present: true, Ready: true},
			},
			wantReady: metav1.ConditionTrue, wantReadyReason: helmv1alpha1.ReasonSuccess,
			wantSynced:      metav1.ConditionFalse,
			wantReconciling: helmv1alpha1.ReasonProgressingWithRetry,
		},
		{
			name: "generation bump voids a stale stalled reason when no attempt runs",
			in: Inputs{
				Generation: 2, Now: testNow,
				Current:    stalledStatus(1, 1, helmv1alpha1.ReasonAuthenticationFailed),
				SecretsErr: writeErr,
			},
			wantReady: metav1.ConditionFalse, wantReadyReason: helmv1alpha1.ReasonAuxiliaryResourcesFailed,
			wantSynced:      metav1.ConditionTrue,
			wantReconciling: helmv1alpha1.ReasonProgressingWithRetry,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Evaluate(tc.in)

			ready := conditionOf(t, got.Status, helmv1alpha1.ConditionTypeReady)
			if ready == nil {
				t.Fatal("Ready condition must always be present")
			}
			if ready.Status != tc.wantReady {
				t.Fatalf("Ready status is %q, want %q", ready.Status, tc.wantReady)
			}
			if ready.Reason != tc.wantReadyReason {
				t.Fatalf("Ready reason is %q, want %q", ready.Reason, tc.wantReadyReason)
			}
			if ready.ObservedGeneration != tc.in.Generation {
				t.Fatalf("Ready observedGeneration is %d, want %d", ready.ObservedGeneration, tc.in.Generation)
			}

			synced := conditionOf(t, got.Status, helmv1alpha1.ConditionTypeSynced)
			switch {
			case tc.wantSynced == "" && synced != nil:
				t.Fatalf("Synced must be absent, got %q", synced.Status)
			case tc.wantSynced != "" && synced == nil:
				t.Fatal("Synced condition is missing")
			case tc.wantSynced != "" && synced.Status != tc.wantSynced:
				t.Fatalf("Synced status is %q, want %q", synced.Status, tc.wantSynced)
			}

			assertAbnormal(t, got.Status, helmv1alpha1.ConditionTypeReconciling, tc.wantReconciling)
			assertAbnormal(t, got.Status, helmv1alpha1.ConditionTypeStalled, tc.wantStalled)

			// I3: the processed generation is always recorded.
			if got.Status.ObservedGeneration != tc.in.Generation {
				t.Fatalf("status.observedGeneration is %d, want %d", got.Status.ObservedGeneration, tc.in.Generation)
			}

			// I1 and I2: exactly one abnormal-true condition while unhealthy, none while healthy.
			healthy := ready.Status == metav1.ConditionTrue && synced != nil && synced.Status == metav1.ConditionTrue
			abnormal := 0
			for _, conditionType := range []string{helmv1alpha1.ConditionTypeReconciling, helmv1alpha1.ConditionTypeStalled} {
				if conditionOf(t, got.Status, conditionType) != nil {
					abnormal++
				}
			}
			if healthy && abnormal != 0 {
				t.Fatalf("healthy repository must carry no abnormal-true conditions, got %d", abnormal)
			}
			if !healthy && abnormal != 1 {
				t.Fatalf("unhealthy repository must carry exactly one abnormal-true condition, got %d", abnormal)
			}
		})
	}
}

// TestEvaluateDecisionErr pins the filter behind Decision.Err: only failures that
// belong on the controller-runtime work queue reach it (auxiliary resources,
// the internal repository object, the chart catalog write). Repository-read
// failures (Fetch, ConfigErr) are deliberately excluded — their retry is
// scheduled through nextSyncTime instead, and routing them into the work queue
// as well would double-schedule the retry.
func TestEvaluateDecisionErr(t *testing.T) {
	secretsErr := errors.New("failed to reconcile secret")
	internalErr := errors.New("failed to reconcile internal repository object")
	catalogWriteErr := errors.New("etcdserver: request timed out")
	fetchErr := errors.New("connection refused")

	cases := []struct {
		name    string
		in      Inputs
		wantErr error
	}{
		{
			name: "auxiliary resource failure reaches Decision.Err",
			in: Inputs{
				Generation: 1, Now: testNow, Current: readyStatus(1),
				SecretsErr: secretsErr,
			},
			wantErr: secretsErr,
		},
		{
			name: "internal repository reconcile failure reaches Decision.Err",
			in: Inputs{
				Generation: 1, Now: testNow, Current: readyStatus(1),
				InternalRepositoryErr: internalErr,
			},
			wantErr: internalErr,
		},
		{
			name: "chart catalog write failure reaches Decision.Err",
			in: Inputs{
				Generation: 1, Now: testNow, Current: readyStatus(1),
				InternalRepository: internalobj.InternalRepositoryState{Present: true, Ready: true},
				Attempted:          true,
				Fetch:              &reposync.FetchOutcome{},
				Catalog:            &reposync.CatalogOutcome{Err: catalogWriteErr},
			},
			wantErr: catalogWriteErr,
		},
		{
			name: "repository read failure never reaches Decision.Err",
			in: Inputs{
				Generation: 1, Now: testNow, Current: readyStatus(1),
				InternalRepository: internalobj.InternalRepositoryState{Present: true, Ready: true},
				Attempted:          true,
				Fetch:              &reposync.FetchOutcome{Err: fetchErr, Message: "cannot read index.yaml"},
			},
			wantErr: nil,
		},
		{
			name: "configuration failure never reaches Decision.Err",
			in: Inputs{
				Generation: 1, Now: testNow, Current: readyStatus(1),
				ConfigErr: &ConfigFailure{
					Reason:  helmv1alpha1.ReasonUnsupportedRepositoryType,
					Message: "unsupported repository schema in use: ftp",
				},
			},
			wantErr: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Evaluate(tc.in)

			if !errors.Is(got.Err, tc.wantErr) {
				t.Fatalf("Decision.Err is %v, want %v", got.Err, tc.wantErr)
			}
		})
	}
}

// TestEvaluatePreservesFailuresWhenNoFetchWasAttempted covers the case where a
// synchronization pass ran (Attempted: true, e.g. its knownCharts read failed
// before the registry was ever contacted) but Fetch is nil: nextFailureCount must
// carry ConsecutiveFetchFailures forward rather than resetting it as if the
// registry had just answered successfully, Ready must not be written
// True/Success off that phantom fetch, and Synced must report the catalog
// failure.
func TestEvaluatePreservesFailuresWhenNoFetchWasAttempted(t *testing.T) {
	catalogErr := errors.New("listing charts of repository \"example\": etcdserver: request timed out")

	in := Inputs{
		Generation: 1,
		Now:        testNow,
		Current: helmv1alpha1.RepositoryStatus{
			ObservedGeneration:       1,
			ConsecutiveFetchFailures: 3,
		},
		InternalRepository: internalobj.InternalRepositoryState{Present: true, Ready: true},
		Attempted:          true,
		Fetch:              nil,
		Catalog:            &reposync.CatalogOutcome{Err: catalogErr},
	}

	got := Evaluate(in)

	if got.Status.ConsecutiveFetchFailures != 3 {
		t.Fatalf("ConsecutiveFetchFailures is %d, want 3 (preserved, not reset by a pass that never reached the registry)",
			got.Status.ConsecutiveFetchFailures)
	}

	ready := conditionOf(t, got.Status, helmv1alpha1.ConditionTypeReady)
	if ready == nil {
		t.Fatal("Ready condition must always be present")
	}
	if ready.Status == metav1.ConditionTrue && ready.Reason == helmv1alpha1.ReasonSuccess {
		t.Fatal("Ready must not be written True/Success off a fetch that was never attempted")
	}

	synced := conditionOf(t, got.Status, helmv1alpha1.ConditionTypeSynced)
	if synced == nil {
		t.Fatal("Synced condition is missing")
	}
	if synced.Status != metav1.ConditionFalse {
		t.Fatalf("Synced status is %q, want False", synced.Status)
	}
	if synced.Reason != helmv1alpha1.ReasonCatalogUpdateFailed {
		t.Fatalf("Synced reason is %q, want %q", synced.Reason, helmv1alpha1.ReasonCatalogUpdateFailed)
	}
}

func TestEvaluatePartialFirstSyncIsNotSynced(t *testing.T) {
	in := Inputs{
		Generation: 1,
		Now:        time.Now().UTC(),
		Attempted:  true,
		Fetch:      &reposync.FetchOutcome{Pending: 2},
		Catalog:    &reposync.CatalogOutcome{},
	}

	decision := Evaluate(in)

	synced := apimeta.FindStatusCondition(decision.Status.Conditions, helmv1alpha1.ConditionTypeSynced)
	if synced == nil || synced.Status != metav1.ConditionFalse {
		t.Fatalf("Synced is %+v, want False", synced)
	}
	if synced.Reason != helmv1alpha1.ReasonPartialSync {
		t.Fatalf("Synced reason is %q, want %q", synced.Reason, helmv1alpha1.ReasonPartialSync)
	}

	ready := apimeta.FindStatusCondition(decision.Status.Conditions, helmv1alpha1.ConditionTypeReady)
	if ready == nil || ready.Status != metav1.ConditionTrue {
		t.Fatalf("Ready is %+v, want True: reading the repository did succeed", ready)
	}
	if cond := apimeta.FindStatusCondition(decision.Status.Conditions, helmv1alpha1.ConditionTypeReconciling); cond != nil {
		t.Fatalf("Reconciling must be absent, got %+v", cond)
	}
	if decision.Status.LastSuccessfulSyncTime != nil {
		t.Fatal("a partial pass must not advance lastSuccessfulSyncTime")
	}
	if decision.Status.ConsecutiveFetchFailures != 0 {
		t.Fatalf("failures are %d, want 0", decision.Status.ConsecutiveFetchFailures)
	}
}

func TestEvaluatePartialSyncAfterFullOneStaysSynced(t *testing.T) {
	earlier := metav1.NewTime(time.Now().UTC().Add(-time.Hour))
	in := Inputs{
		Generation: 1,
		Now:        time.Now().UTC(),
		Attempted:  true,
		Fetch:      &reposync.FetchOutcome{Pending: 1},
		Catalog:    &reposync.CatalogOutcome{},
		Current: helmv1alpha1.RepositoryStatus{
			ObservedGeneration:     1,
			LastSuccessfulSyncTime: &earlier,
		},
	}

	decision := Evaluate(in)

	synced := apimeta.FindStatusCondition(decision.Status.Conditions, helmv1alpha1.ConditionTypeSynced)
	if synced == nil || synced.Status != metav1.ConditionTrue {
		t.Fatalf("Synced is %+v, want True", synced)
	}
	if !decision.Status.LastSuccessfulSyncTime.Equal(&earlier) {
		t.Fatalf("lastSuccessfulSyncTime is %v, want it unchanged at %v", decision.Status.LastSuccessfulSyncTime, earlier)
	}
}

func TestEvaluatePartialSyncNeverStalls(t *testing.T) {
	current := helmv1alpha1.RepositoryStatus{ObservedGeneration: 1}

	for range MaxFetchFailures + 2 {
		decision := Evaluate(Inputs{
			Generation: 1,
			Now:        time.Now().UTC(),
			Attempted:  true,
			Fetch:      &reposync.FetchOutcome{Pending: 1},
			Catalog:    &reposync.CatalogOutcome{},
			Current:    current,
		})
		current = decision.Status
	}

	if cond := apimeta.FindStatusCondition(current.Conditions, helmv1alpha1.ConditionTypeStalled); cond != nil {
		t.Fatalf("repeated partial passes must not stall the repository, got %+v", cond)
	}
	if current.ConsecutiveFetchFailures != 0 {
		t.Fatalf("failures are %d, want 0", current.ConsecutiveFetchFailures)
	}
}

func TestEvaluateFullSyncAdvancesLastSuccessfulSyncTime(t *testing.T) {
	now := time.Now().UTC()
	decision := Evaluate(Inputs{
		Generation: 1,
		Now:        now,
		Attempted:  true,
		Fetch:      &reposync.FetchOutcome{},
		Catalog:    &reposync.CatalogOutcome{},
	})

	if decision.Status.LastSuccessfulSyncTime == nil || !decision.Status.LastSuccessfulSyncTime.Time.Equal(now) {
		t.Fatalf("lastSuccessfulSyncTime is %v, want %v", decision.Status.LastSuccessfulSyncTime, now)
	}
	if decision.Status.LastSyncedGeneration != 1 {
		t.Fatalf("lastSyncedGeneration is %d, want 1", decision.Status.LastSyncedGeneration)
	}
}

// TestEvaluateLastSyncedGenerationSurvivesAFailedPass pins the property releases
// depend on: the field says which spec the published catalog belongs to, so a pass
// that failed without touching the catalog must leave it alone. The Synced
// condition cannot answer that question — it is the verdict of the latest pass, and
// goes False over a catalog that is complete and current.
func TestEvaluateLastSyncedGenerationSurvivesAFailedPass(t *testing.T) {
	earlier := metav1.NewTime(time.Now().UTC().Add(-time.Hour))
	published := helmv1alpha1.RepositoryStatus{
		ObservedGeneration:     1,
		LastSyncedGeneration:   1,
		LastSuccessfulSyncTime: &earlier,
	}

	decision := Evaluate(Inputs{
		Generation: 1,
		Now:        time.Now().UTC(),
		Attempted:  true,
		Fetch:      &reposync.FetchOutcome{Err: errors.New("the index request failed")},
		Current:    published,
	})

	synced := apimeta.FindStatusCondition(decision.Status.Conditions, helmv1alpha1.ConditionTypeSynced)
	if synced == nil || synced.Status != metav1.ConditionFalse {
		t.Fatalf("Synced is %+v, want False: the pass did fail", synced)
	}
	if decision.Status.LastSyncedGeneration != 1 {
		t.Fatalf("lastSyncedGeneration is %d, want it left at 1", decision.Status.LastSyncedGeneration)
	}

	// The same failure after a spec change must not claim the new generation
	// either: the catalog on record is still the previous url's.
	edited := Evaluate(Inputs{
		Generation: 2,
		Now:        time.Now().UTC(),
		Attempted:  true,
		Fetch:      &reposync.FetchOutcome{Err: errors.New("the index request failed")},
		Current:    published,
	})

	if edited.Status.LastSyncedGeneration != 1 {
		t.Fatalf("lastSyncedGeneration is %d, want it left at 1 after a failed resync", edited.Status.LastSyncedGeneration)
	}
}

// TestEvaluatePendingTagsStillPublishACatalog pins the distinction between the two
// fields: a tag the registry has not answered about leaves the repository not fully
// mirrored, but the catalog still lists every version that exists, which is the
// only question releases ask of it. A repository with one permanently unreadable
// tag must not hold every release on it in limbo.
func TestEvaluatePendingTagsStillPublishACatalog(t *testing.T) {
	decision := Evaluate(Inputs{
		Generation: 2,
		Now:        time.Now().UTC(),
		Attempted:  true,
		Fetch:      &reposync.FetchOutcome{Pending: 1},
		Catalog:    &reposync.CatalogOutcome{},
		Current:    helmv1alpha1.RepositoryStatus{ObservedGeneration: 1, LastSyncedGeneration: 1},
	})

	if decision.Status.LastSyncedGeneration != 2 {
		t.Fatalf("lastSyncedGeneration is %d, want 2: the catalog was published", decision.Status.LastSyncedGeneration)
	}
	if decision.Status.LastSuccessfulSyncTime != nil {
		t.Fatal("lastSuccessfulSyncTime must stay unset: the repository is not mirrored in full")
	}
}

// TestEvaluateLastSyncedGenerationWaitsForTheCatalog pins the other half: a spec
// change does not carry the previous catalog's generation forward, and neither does
// a pass that read the repository but could not write the catalog.
func TestEvaluateLastSyncedGenerationWaitsForTheCatalog(t *testing.T) {
	published := helmv1alpha1.RepositoryStatus{ObservedGeneration: 1, LastSyncedGeneration: 1}

	edited := Evaluate(Inputs{
		Generation: 2,
		Now:        time.Now().UTC(),
		Attempted:  true,
		Fetch:      &reposync.FetchOutcome{},
		Catalog:    &reposync.CatalogOutcome{Err: errors.New("the catalog write failed")},
		Current:    published,
	})

	if edited.Status.LastSyncedGeneration != 1 {
		t.Fatalf("lastSyncedGeneration is %d, want it still 1: generation 2 has no catalog yet", edited.Status.LastSyncedGeneration)
	}

	written := Evaluate(Inputs{
		Generation: 2,
		Now:        time.Now().UTC(),
		Attempted:  true,
		Fetch:      &reposync.FetchOutcome{},
		Catalog:    &reposync.CatalogOutcome{},
		Current:    published,
	})

	if written.Status.LastSyncedGeneration != 2 {
		t.Fatalf("lastSyncedGeneration is %d, want 2", written.Status.LastSyncedGeneration)
	}
}

// TestEvaluateChartCount pins what the field means: how many charts the catalog
// holds for the repository as of the last successful read. A pointer, so that a
// repository never read apart from one offering nothing is not reported as offering
// nothing, and a failed read leaves the last known answer rather than replacing it
// with zero.
func TestEvaluateChartCount(t *testing.T) {
	never := Evaluate(Inputs{Generation: 1, Now: testNow})
	if never.Status.ChartCount != nil {
		t.Fatalf("chartCount is %v before any read, want nil", *never.Status.ChartCount)
	}

	read := Evaluate(Inputs{
		Generation: 1, Now: testNow,
		Attempted: true,
		Fetch:     &reposync.FetchOutcome{Charts: 3},
		Catalog:   &reposync.CatalogOutcome{},
	})
	if read.Status.ChartCount == nil || *read.Status.ChartCount != 3 {
		t.Fatalf("chartCount is %v, want 3", read.Status.ChartCount)
	}

	empty := Evaluate(Inputs{
		Generation: 1, Now: testNow,
		Attempted: true,
		Fetch:     &reposync.FetchOutcome{},
		Catalog:   &reposync.CatalogOutcome{},
	})
	if empty.Status.ChartCount == nil || *empty.Status.ChartCount != 0 {
		t.Fatalf("chartCount is %v for a repository offering nothing, want 0", empty.Status.ChartCount)
	}

	failed := Evaluate(Inputs{
		Generation: 1, Now: testNow,
		Current:   read.Status,
		Attempted: true,
		Fetch:     &reposync.FetchOutcome{Err: errors.New("connection refused"), Reason: helmv1alpha1.ReasonSyncFailed},
	})
	if failed.Status.ChartCount == nil || *failed.Status.ChartCount != 3 {
		t.Fatalf("chartCount is %v after a failed read, want the last successful 3", failed.Status.ChartCount)
	}
}

// TestEvaluateChartCountDescribesTheReadWhenTheCatalogUpdateFails pins what the
// field falls back to when the catalog write does not finish. The catalog gives up
// on the first object it cannot write, so it may have published nothing at all,
// while chartCount still reports what the read found: the number describes the
// repository, not the objects in the cluster. Keeping the read's answer is what
// makes a failed pass readable — the user sees the size of the index the retry has
// to publish — and the failing Synced condition is what says the catalog is behind
// it.
func TestEvaluateChartCountDescribesTheReadWhenTheCatalogUpdateFails(t *testing.T) {
	decision := Evaluate(Inputs{
		Generation: 1, Now: testNow,
		Attempted: true,
		Fetch:     &reposync.FetchOutcome{Charts: 7},
		Catalog:   &reposync.CatalogOutcome{Err: errors.New("creating or updating chart d8/nginx: etcdserver: request timed out")},
	})

	if decision.Status.ChartCount == nil || *decision.Status.ChartCount != 7 {
		t.Fatalf("chartCount is %v after a failed catalog update, want the 7 charts the read found", decision.Status.ChartCount)
	}

	synced := conditionOf(t, decision.Status, helmv1alpha1.ConditionTypeSynced)
	if synced == nil || synced.Status != metav1.ConditionFalse ||
		synced.Reason != helmv1alpha1.ReasonCatalogUpdateFailed {
		t.Fatalf("Synced is %+v, want False/%s to explain the count", synced, helmv1alpha1.ReasonCatalogUpdateFailed)
	}
	if decision.Status.LastSuccessfulSyncTime != nil {
		t.Fatalf("lastSuccessfulSyncTime is %v, want none: the catalog was not brought up to date",
			decision.Status.LastSuccessfulSyncTime)
	}
}

// TestEvaluateAccountsForTheChartsTheCatalogCannotPublish pins the one thing that
// makes chartCount checkable: it is the number of chart objects the user can find in
// the cluster, not the length of the repository's index. A chart whose name the API
// cannot record never becomes an object, so counting it would leave a difference
// nothing in the status explains — and Synced is where the explanation goes.
func TestEvaluateAccountsForTheChartsTheCatalogCannotPublish(t *testing.T) {
	cases := []struct {
		name        string
		charts      int
		skipped     []string
		wantCount   int32
		wantSynced  metav1.ConditionStatus
		wantReason  string
		wantMessage []string
	}{
		{
			name:       "a catalog that holds every chart reports the whole index and is synced",
			charts:     3,
			wantCount:  3,
			wantSynced: metav1.ConditionTrue,
			wantReason: helmv1alpha1.ReasonSuccess,
		},
		{
			name:        "a skipped chart is subtracted and named",
			charts:      3,
			skipped:     []string{"my chart"},
			wantCount:   2,
			wantSynced:  metav1.ConditionFalse,
			wantReason:  helmv1alpha1.ReasonPartialSync,
			wantMessage: []string{"leaves out 1 chart whose", `"my chart"`},
		},
		{
			// A message is capped at 32768 bytes by the API server, so an index that
			// names nothing the API accepts must not make the status unwritable.
			name:        "more skipped charts than fit in a message are counted in full and named in part",
			charts:      5,
			skipped:     []string{"a b", "c d", "e f", "g h", "i j"},
			wantCount:   0,
			wantSynced:  metav1.ConditionFalse,
			wantReason:  helmv1alpha1.ReasonPartialSync,
			wantMessage: []string{"leaves out 5 charts whose", `"a b", "c d", "e f" and 2 more`},
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			decision := Evaluate(Inputs{
				Generation: 1, Now: testNow,
				Attempted: true,
				Fetch:     &reposync.FetchOutcome{Charts: tt.charts},
				Catalog:   &reposync.CatalogOutcome{SkippedCharts: tt.skipped},
			})

			if decision.Status.ChartCount == nil {
				t.Fatalf("chartCount is nil after a successful read, want %d", tt.wantCount)
			}
			if *decision.Status.ChartCount != tt.wantCount {
				t.Fatalf("chartCount is %d, want %d", *decision.Status.ChartCount, tt.wantCount)
			}

			synced := conditionOf(t, decision.Status, helmv1alpha1.ConditionTypeSynced)
			if synced == nil || synced.Status != tt.wantSynced || synced.Reason != tt.wantReason {
				t.Fatalf("Synced is %+v, want %s/%s", synced, tt.wantSynced, tt.wantReason)
			}
			for _, want := range tt.wantMessage {
				if !strings.Contains(synced.Message, want) {
					t.Errorf("Synced message is %q, want it to contain %q", synced.Message, want)
				}
			}

			// A skipped chart is not a failure: the pass read the repository and wrote
			// everything it could, so nothing about it may escalate.
			if ready := conditionOf(t, decision.Status, helmv1alpha1.ConditionTypeReady); ready == nil ||
				ready.Status != metav1.ConditionTrue {
				t.Fatalf("Ready is %+v, want True", ready)
			}
			assertAbnormal(t, decision.Status, helmv1alpha1.ConditionTypeStalled, "")
			assertAbnormal(t, decision.Status, helmv1alpha1.ConditionTypeReconciling, "")
			if decision.Status.LastSuccessfulSyncTime == nil {
				t.Fatal("a pass that published everything it could must advance lastSuccessfulSyncTime")
			}
			if decision.Err != nil {
				t.Fatalf("decision error is %v, want none", decision.Err)
			}
		})
	}
}

// TestEvaluateSkippedChartsAreReportedWithoutChurn pins that saying it on every pass
// is free. The catalog is re-read every few minutes and the verdict on a name the API
// cannot hold never changes, so the report would be noise if each pass rewrote the
// condition; an unchanged reason and message leave the transition time alone.
func TestEvaluateSkippedChartsAreReportedWithoutChurn(t *testing.T) {
	in := Inputs{
		Generation: 1, Now: testNow,
		Attempted: true,
		Fetch:     &reposync.FetchOutcome{Charts: 2},
		Catalog:   &reposync.CatalogOutcome{SkippedCharts: []string{"my chart"}},
	}

	first := Evaluate(in)

	in.Current = first.Status
	in.Now = testNow.Add(SyncInterval)
	second := Evaluate(in)

	before := conditionOf(t, first.Status, helmv1alpha1.ConditionTypeSynced)
	after := conditionOf(t, second.Status, helmv1alpha1.ConditionTypeSynced)
	if before == nil || after == nil {
		t.Fatalf("Synced is missing: %+v, %+v", before, after)
	}
	if !after.LastTransitionTime.Equal(&before.LastTransitionTime) {
		t.Fatalf("Synced transition time moved from %v to %v on an unchanged verdict",
			before.LastTransitionTime, after.LastTransitionTime)
	}
	if after.Message != before.Message {
		t.Fatalf("Synced message changed from %q to %q", before.Message, after.Message)
	}
}

// TestEvaluateReportsSkippedChartsAndUnresolvedVersionsTogether pins that the two
// ways a catalog can fall short of the repository do not hide each other: they share
// one condition, and a user fixing one must still see the other.
func TestEvaluateReportsSkippedChartsAndUnresolvedVersionsTogether(t *testing.T) {
	decision := Evaluate(Inputs{
		Generation: 1, Now: testNow,
		Attempted: true,
		Fetch:     &reposync.FetchOutcome{Charts: 4, Pending: 2},
		Catalog:   &reposync.CatalogOutcome{SkippedCharts: []string{"my chart"}},
	})

	synced := conditionOf(t, decision.Status, helmv1alpha1.ConditionTypeSynced)
	if synced == nil || synced.Reason != helmv1alpha1.ReasonPartialSync {
		t.Fatalf("Synced is %+v, want %s", synced, helmv1alpha1.ReasonPartialSync)
	}
	for _, want := range []string{`"my chart"`, "left 2 chart versions unresolved"} {
		if !strings.Contains(synced.Message, want) {
			t.Errorf("Synced message is %q, want it to contain %q", synced.Message, want)
		}
	}
}

// TestEvaluateDescribesTheSameSkippedChartsTheSameWay pins that the message depends
// on the set of skipped charts and not on the order they arrive in. A Helm index is
// a map on the wire, so two passes over an unchanged repository hand the same names
// over in different orders; if that reached the message, every pass would rewrite
// the condition and patch the repository over a state that never changed. The
// transition time would stay put — it follows the status — but the write would not.
func TestEvaluateDescribesTheSameSkippedChartsTheSameWay(t *testing.T) {
	cases := []struct {
		name  string
		order []string
	}{
		{name: "two skipped charts in one order", order: []string{"my chart", "_internal"}},
		{name: "the same two in the other order", order: []string{"_internal", "my chart"}},
	}

	var want string

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			got := messageFor(t, tt.order)
			if want == "" {
				want = got
			}
			if got != want {
				t.Fatalf("Synced message is %q, want %q whatever order the names arrive in", got, want)
			}
			for _, name := range tt.order {
				if !strings.Contains(got, strconv.Quote(name)) {
					t.Errorf("Synced message is %q, want it to name %q", got, name)
				}
			}
		})
	}
}

// TestEvaluateNamesTheSameSkippedChartsWhateverOrderTheyArriveIn pins the same
// property where it can actually break: with more names than the message shows, the
// order decides which ones are left out, not just how they are spelled. Two passes
// over an unchanged repository must still pick the same three.
func TestEvaluateNamesTheSameSkippedChartsWhateverOrderTheyArriveIn(t *testing.T) {
	cases := []struct {
		name  string
		order []string
	}{
		{name: "five skipped charts as one read returned them", order: []string{"e f", "a b", "d e", "b c", "c d"}},
		{name: "the same five as the next read returned them", order: []string{"c d", "b c", "e f", "a b", "d e"}},
		{name: "and once more, reversed", order: []string{"e f", "d e", "c d", "b c", "a b"}},
	}

	var want string

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			got := messageFor(t, tt.order)
			if want == "" {
				want = got
			}
			if got != want {
				t.Fatalf("Synced message is %q, want %q whatever order the names arrive in", got, want)
			}
			if !strings.Contains(got, `"a b", "b c", "c d" and 2 more`) {
				t.Fatalf("Synced message is %q, want the three names that sort first", got)
			}
		})
	}
}

// TestEvaluateDoesNotMutateTheSkippedChartsItIsGiven pins that Evaluate is a pure
// function of its inputs. The outcome it reads belongs to the caller, which logs it
// and hands it on; sorting the names in place would reorder somebody else's slice as
// a side effect of writing a status message.
func TestEvaluateDoesNotMutateTheSkippedChartsItIsGiven(t *testing.T) {
	skipped := []string{"my chart", "_internal", "z chart", "a chart"}
	before := slices.Clone(skipped)

	Evaluate(Inputs{
		Generation: 1, Now: testNow,
		Attempted: true,
		Fetch:     &reposync.FetchOutcome{Charts: 4},
		Catalog:   &reposync.CatalogOutcome{SkippedCharts: skipped},
	})

	if !slices.Equal(skipped, before) {
		t.Fatalf("Evaluate reordered the skipped charts it was given: %q, want %q", skipped, before)
	}
}

// TestEvaluateNamesChartsThatAreNotPlainASCII pins that a name the API rejects is
// still safe to put in a message. Such a name is arbitrary text: it may be built
// from multi-byte runes, which must not be cut in half or mangled, and it may hold
// bytes that quoting turns into escapes, which must stay escaped so the message
// remains one readable line.
func TestEvaluateNamesChartsThatAreNotPlainASCII(t *testing.T) {
	cases := []struct {
		name    string
		skipped string
		want    string
	}{
		{
			name:    "a name of multi-byte runes is shown whole",
			skipped: "диаграмма-графики",
			want:    `"диаграмма-графики"`,
		},
		{
			name: "a long name of multi-byte runes is cut on a rune boundary",
			// 200 runes, 400 bytes: past maxChartNameRunes either way, so a cut made
			// in bytes would land inside a character.
			skipped: strings.Repeat("я", 200),
			want:    `"` + strings.Repeat("я", maxChartNameRunes) + `..."`,
		},
		{
			name:    "characters that quoting escapes stay escaped",
			skipped: "chart\n\t\"quoted\"\x00",
			want:    `"chart\n\t\"quoted\"\x00"`,
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			got := messageFor(t, []string{tt.skipped})
			if !strings.Contains(got, tt.want) {
				t.Fatalf("Synced message is %q, want it to contain %q", got, tt.want)
			}
			if strings.ContainsAny(got, "\n\t\x00") {
				t.Fatalf("Synced message is %q, want no raw control characters in it", got)
			}
			if !utf8.ValidString(got) {
				t.Fatalf("Synced message %q is not valid UTF-8", got)
			}
		})
	}
}

// messageFor returns the Synced message of a pass that read the repository and
// skipped the given charts.
func messageFor(t *testing.T, skipped []string) string {
	t.Helper()

	decision := Evaluate(Inputs{
		Generation: 1, Now: testNow,
		Attempted: true,
		Fetch:     &reposync.FetchOutcome{Charts: len(skipped)},
		Catalog:   &reposync.CatalogOutcome{SkippedCharts: skipped},
	})

	synced := conditionOf(t, decision.Status, helmv1alpha1.ConditionTypeSynced)
	if synced == nil || synced.Reason != helmv1alpha1.ReasonPartialSync {
		t.Fatalf("Synced is %+v, want %s", synced, helmv1alpha1.ReasonPartialSync)
	}

	return synced.Message
}

// TestEvaluateBoundsTheLengthOfANamedChart pins the other half of the message
// bound. Limiting how many names are shown is not enough: an index may publish
// names far longer than anything a condition can hold, and a message the API server
// rejects would cost the repository every other verdict in the same status write.
func TestEvaluateBoundsTheLengthOfANamedChart(t *testing.T) {
	// Not a label value, so it is skipped for its grammar as well as its length,
	// and long enough that three of them would blow past the 32768-byte limit.
	huge := strings.Repeat("a b", 20000)

	decision := Evaluate(Inputs{
		Generation: 1, Now: testNow,
		Attempted: true,
		Fetch:     &reposync.FetchOutcome{Charts: 3},
		Catalog:   &reposync.CatalogOutcome{SkippedCharts: []string{huge, huge + "c", huge + "d"}},
	})

	synced := conditionOf(t, decision.Status, helmv1alpha1.ConditionTypeSynced)
	if synced == nil || synced.Reason != helmv1alpha1.ReasonPartialSync {
		t.Fatalf("Synced is %+v, want %s", synced, helmv1alpha1.ReasonPartialSync)
	}
	// The limit the API server enforces on a condition message.
	if len(synced.Message) > 32768 {
		t.Fatalf("Synced message is %d bytes, want at most 32768", len(synced.Message))
	}
	if !strings.Contains(synced.Message, "leaves out 3 charts whose") {
		t.Fatalf("Synced message is %q, want it to still count every skipped chart", synced.Message)
	}
}

func assertAbnormal(t *testing.T, status helmv1alpha1.RepositoryStatus, conditionType, wantReason string) {
	t.Helper()

	cond := conditionOf(t, status, conditionType)
	if wantReason == "" {
		if cond != nil {
			t.Fatalf("%s must be absent, got reason %q", conditionType, cond.Reason)
		}

		return
	}

	if cond == nil {
		t.Fatalf("%s is missing, want reason %q", conditionType, wantReason)
	}
	if cond.Status != metav1.ConditionTrue {
		t.Fatalf("%s status is %q, want True", conditionType, cond.Status)
	}
	if cond.Reason != wantReason {
		t.Fatalf("%s reason is %q, want %q", conditionType, cond.Reason, wantReason)
	}
}

// TestEvaluateForcedAttemptStampsLastForceReconcileTime pins the contract that
// makes the field readable: the stamp is written exactly on the pass that
// consumes the force annotation, which is the pass that attempted.
func TestEvaluateForcedAttemptStampsLastForceReconcileTime(t *testing.T) {
	now := time.Now().UTC()
	decision := Evaluate(Inputs{
		Generation: 1,
		Now:        now,
		Forced:     true,
		Attempted:  true,
		Fetch:      &reposync.FetchOutcome{},
		Catalog:    &reposync.CatalogOutcome{},
	})

	if decision.Status.LastForceReconcileTime == nil || !decision.Status.LastForceReconcileTime.Time.Equal(now) {
		t.Fatalf("lastForceReconcileTime is %v, want %v", decision.Status.LastForceReconcileTime, now)
	}
}

// TestEvaluateFailedForcedAttemptStampsLastForceReconcileTime: the stamp answers
// "when was my force request processed", not "when did it succeed". A forced pass
// whose read failed still consumes the annotation, so it must still be stamped —
// otherwise an empty field could not be told apart from a request never made.
func TestEvaluateFailedForcedAttemptStampsLastForceReconcileTime(t *testing.T) {
	now := time.Now().UTC()
	decision := Evaluate(Inputs{
		Generation: 1,
		Now:        now,
		Forced:     true,
		Attempted:  true,
		Fetch: &reposync.FetchOutcome{
			Err:     errors.New("dial tcp: connection refused"),
			Reason:  helmv1alpha1.ReasonSyncFailed,
			Message: "failed to read the repository",
		},
	})

	if decision.Status.LastForceReconcileTime == nil || !decision.Status.LastForceReconcileTime.Time.Equal(now) {
		t.Fatalf("lastForceReconcileTime is %v, want %v", decision.Status.LastForceReconcileTime, now)
	}
}

// TestEvaluateForcedPassWithoutAttemptKeepsLastForceReconcileTime covers the pass
// that carries a force request it could not act on — a structural failure stops it
// before the attempt, so the annotation survives and the stamp must not move.
func TestEvaluateForcedPassWithoutAttemptKeepsLastForceReconcileTime(t *testing.T) {
	previous := metav1.NewTime(testNow.Add(-time.Hour))
	current := readyStatus(1)
	current.LastForceReconcileTime = &previous

	decision := Evaluate(Inputs{
		Generation: 1,
		Now:        testNow,
		Current:    current,
		Forced:     true,
		SecretsErr: errors.New("creating auth secret: forbidden"),
	})

	if decision.Status.LastForceReconcileTime == nil ||
		!decision.Status.LastForceReconcileTime.Time.Equal(previous.Time) {
		t.Fatalf("lastForceReconcileTime is %v, want it kept at %v",
			decision.Status.LastForceReconcileTime, previous.Time)
	}
}

// TestEvaluateScheduledSyncKeepsLastForceReconcileTime is the complement: an
// ordinary scheduled synchronization must not touch the stamp, or it would
// report a force request that was never made.
func TestEvaluateScheduledSyncKeepsLastForceReconcileTime(t *testing.T) {
	previous := metav1.NewTime(testNow.Add(-time.Hour))
	current := readyStatus(1)
	current.LastForceReconcileTime = &previous

	decision := Evaluate(Inputs{
		Generation: 1,
		Now:        testNow,
		Current:    current,
		Attempted:  true,
		Fetch:      &reposync.FetchOutcome{},
		Catalog:    &reposync.CatalogOutcome{},
	})

	if decision.Status.LastForceReconcileTime == nil ||
		!decision.Status.LastForceReconcileTime.Time.Equal(previous.Time) {
		t.Fatalf("lastForceReconcileTime is %v, want it kept at %v",
			decision.Status.LastForceReconcileTime, previous.Time)
	}
}

// TestEvaluateSchedulesAnotherPassAfterAnIncompleteRead pins how a pass that read the
// repository but ran out of its own budget is reported. It is not a failure — the
// catalog advanced and the failure counter must not move towards Stalled — and it is
// not a finished synchronization either: lastSyncedGeneration says which spec the
// published catalog answers for, and this catalog answers for only part of it, so a
// release asking about a version nobody examined has to wait rather than be told the
// repository does not offer it. The next pass is due well before the normal interval,
// because it is what finishes this one.
func TestEvaluateSchedulesAnotherPassAfterAnIncompleteRead(t *testing.T) {
	decision := Evaluate(Inputs{
		Generation: 2,
		Now:        testNow,
		Attempted:  true,
		Fetch:      &reposync.FetchOutcome{Charts: 1, Unexamined: 400},
		Catalog:    &reposync.CatalogOutcome{},
		Current:    helmv1alpha1.RepositoryStatus{ObservedGeneration: 2, LastSyncedGeneration: 1, ConsecutiveFetchFailures: 0},
	})

	if decision.Err != nil {
		t.Fatalf("an incomplete read is not an error, got %v", decision.Err)
	}
	if decision.Status.ConsecutiveFetchFailures != 0 {
		t.Fatalf("failures = %d, want 0: the read succeeded", decision.Status.ConsecutiveFetchFailures)
	}
	if apimeta.IsStatusConditionTrue(decision.Status.Conditions, helmv1alpha1.ConditionTypeStalled) {
		t.Fatal("an incomplete read must never stall the repository")
	}
	if !apimeta.IsStatusConditionTrue(decision.Status.Conditions, helmv1alpha1.ConditionTypeReady) {
		t.Fatal("the repository was read and is usable: Ready must stay true")
	}

	synced := apimeta.FindStatusCondition(decision.Status.Conditions, helmv1alpha1.ConditionTypeSynced)
	if synced == nil || synced.Status != metav1.ConditionFalse || synced.Reason != helmv1alpha1.ReasonPartialSync {
		t.Fatalf("synced = %+v, want a false PartialSync verdict", synced)
	}
	if !strings.Contains(synced.Message, "400") {
		t.Fatalf("synced message %q must say how much is left", synced.Message)
	}

	if decision.Status.LastSyncedGeneration != 1 {
		t.Fatalf("lastSyncedGeneration = %d, want it held at 1: the catalog does not answer for this spec yet",
			decision.Status.LastSyncedGeneration)
	}
	if decision.Status.LastSuccessfulSyncTime != nil {
		t.Fatal("lastSuccessfulSyncTime must stay unset: the repository is not mirrored in full")
	}

	if decision.RequeueAfter > SyncInterval/2 {
		t.Fatalf("requeueAfter = %v, want the short incomplete-pass delay, not the full interval", decision.RequeueAfter)
	}
	if decision.Status.NextSyncTime == nil ||
		decision.Status.NextSyncTime.Sub(testNow) > IncompleteSyncDelay+withJitter(IncompleteSyncDelay, SyncBackoffJitter) {
		t.Fatalf("nextSyncTime = %v, want about %v after now", decision.Status.NextSyncTime, IncompleteSyncDelay)
	}
}

// TestEvaluateCompletesTheGenerationOnceNothingIsLeftUnexamined is the other half:
// the hold on lastSyncedGeneration is released by the pass that covers the repository,
// so the convergence actually ends.
func TestEvaluateCompletesTheGenerationOnceNothingIsLeftUnexamined(t *testing.T) {
	decision := Evaluate(Inputs{
		Generation: 2,
		Now:        testNow,
		Attempted:  true,
		Fetch:      &reposync.FetchOutcome{Charts: 1},
		Catalog:    &reposync.CatalogOutcome{},
		Current:    helmv1alpha1.RepositoryStatus{ObservedGeneration: 2, LastSyncedGeneration: 1},
	})

	if decision.Status.LastSyncedGeneration != 2 {
		t.Fatalf("lastSyncedGeneration = %d, want 2", decision.Status.LastSyncedGeneration)
	}
	if !apimeta.IsStatusConditionTrue(decision.Status.Conditions, helmv1alpha1.ConditionTypeSynced) {
		t.Fatal("a pass that examined everything must report a completed synchronization")
	}
	if decision.RequeueAfter < SyncInterval/2 {
		t.Fatalf("requeueAfter = %v, want the normal interval", decision.RequeueAfter)
	}
}
