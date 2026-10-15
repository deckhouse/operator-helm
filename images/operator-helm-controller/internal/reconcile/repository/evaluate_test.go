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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

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
		wantReconciling string
		wantStalled     string
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
			name: "synced carries the evidence when Ready was owned by another rule",
			in: Inputs{
				Generation: 1, Now: testNow, Current: syncedNotReadyStatus(1),
				InternalRepository: internalobj.InternalRepositoryState{Present: true, Ready: true},
			},
			wantReady: metav1.ConditionTrue, wantReadyReason: helmv1alpha1.ReasonSuccess,
			wantSynced: metav1.ConditionTrue,
		},
		{
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

			if got.Status.ObservedGeneration != tc.in.Generation {
				t.Fatalf("status.observedGeneration is %d, want %d", got.Status.ObservedGeneration, tc.in.Generation)
			}

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
			name:    "a long name of multi-byte runes is cut on a rune boundary",
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

func TestEvaluateBoundsTheLengthOfANamedChart(t *testing.T) {
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

func TestEvaluateSchedulesTheNextPassFromTheEndOfTheRead(t *testing.T) {
	const readDuration = 3 * time.Minute

	syncEnd := testNow.Add(readDuration)

	decision := Evaluate(Inputs{
		Generation: 1,
		Now:        testNow,
		SyncEnd:    syncEnd,
		Attempted:  true,
		Fetch:      &reposync.FetchOutcome{Charts: 1, Unexamined: 400},
		Catalog:    &reposync.CatalogOutcome{},
		Current:    helmv1alpha1.RepositoryStatus{ObservedGeneration: 1},
	})

	if decision.Status.NextSyncTime == nil || !decision.Status.NextSyncTime.After(syncEnd) {
		t.Fatalf("nextSyncTime = %v, want it after the end of the read at %v",
			decision.Status.NextSyncTime, syncEnd)
	}
	if got := decision.Status.NextSyncTime.Sub(syncEnd); got > IncompleteSyncDelay {
		t.Fatalf("nextSyncTime is %v after the read, want at most the %v delay", got, IncompleteSyncDelay)
	}
	if decision.RequeueAfter < IncompleteSyncDelay/2 {
		t.Fatalf("requeueAfter = %v, want the pause the delay promises, not what is left of it after the read",
			decision.RequeueAfter)
	}
}

func TestEvaluateSchedulesFromNowWhenNoAttemptRan(t *testing.T) {
	next := testNow.Add(2 * time.Minute)

	decision := Evaluate(Inputs{
		Generation: 1,
		Now:        testNow,
		Current: helmv1alpha1.RepositoryStatus{
			ObservedGeneration: 1,
			NextSyncTime:       &metav1.Time{Time: next},
		},
	})

	if got := decision.RequeueAfter; got != next.Sub(testNow) {
		t.Fatalf("requeueAfter = %v, want %v: the schedule of a pass that read nothing is unchanged",
			got, next.Sub(testNow))
	}
}

func TestEvaluateReportsAnIncompleteCatalogAfterAGenerationSynced(t *testing.T) {
	decision := Evaluate(Inputs{
		Generation: 1,
		Now:        testNow,
		Attempted:  true,
		Fetch:      &reposync.FetchOutcome{Charts: 1, Unexamined: 400},
		Catalog:    &reposync.CatalogOutcome{},
		Current: helmv1alpha1.RepositoryStatus{
			ObservedGeneration:   1,
			LastSyncedGeneration: 1,
		},
	})

	if decision.Status.LastSyncedGeneration != 1 {
		t.Fatalf("lastSyncedGeneration = %d, want it left at 1: this is the case it cannot report",
			decision.Status.LastSyncedGeneration)
	}
	if decision.Status.UnexaminedVersions != 400 {
		t.Fatalf("unexaminedVersions = %d, want 400: the catalog does not list everything the repository offers",
			decision.Status.UnexaminedVersions)
	}
}

func TestEvaluateClearsTheIncompleteCatalogOnceAPassCoversTheRepository(t *testing.T) {
	decision := Evaluate(Inputs{
		Generation: 1,
		Now:        testNow,
		Attempted:  true,
		Fetch:      &reposync.FetchOutcome{Charts: 1},
		Catalog:    &reposync.CatalogOutcome{},
		Current: helmv1alpha1.RepositoryStatus{
			ObservedGeneration:   1,
			LastSyncedGeneration: 1,
			UnexaminedVersions:   400,
		},
	})

	if decision.Status.UnexaminedVersions != 0 {
		t.Fatalf("unexaminedVersions = %d, want 0", decision.Status.UnexaminedVersions)
	}
}

func TestEvaluateKeepsTheIncompleteCatalogThroughAFailedPass(t *testing.T) {
	decision := Evaluate(Inputs{
		Generation: 1,
		Now:        testNow,
		Attempted:  true,
		Fetch:      &reposync.FetchOutcome{Err: errors.New("registry unreachable"), Message: "Failed to read the repository catalog"},
		Catalog:    &reposync.CatalogOutcome{},
		Current: helmv1alpha1.RepositoryStatus{
			ObservedGeneration:   1,
			LastSyncedGeneration: 1,
			UnexaminedVersions:   400,
		},
	})

	if decision.Status.UnexaminedVersions != 400 {
		t.Fatalf("unexaminedVersions = %d, want it kept at 400: a failed pass published no catalog",
			decision.Status.UnexaminedVersions)
	}
}

func TestEvaluateReportsACatalogWriteRejectedByItsLockAsALostRace(t *testing.T) {
	conflict := apierrors.NewConflict(
		schema.GroupResource{Group: helmv1alpha1.GroupName, Resource: "helmapplicationcharts"},
		"stable-podinfo",
		errors.New("the object has been modified; please apply your changes to the latest version and try again"),
	)

	tests := []struct {
		name              string
		err               error
		wantMessage       string
		wantShortSchedule bool
	}{
		{
			name:              "a rejected patch is named as the race it is and retried promptly",
			err:               conflict,
			wantMessage:       "The chart catalog changed since this pass read it; retrying",
			wantShortSchedule: true,
		},
		{
			name:              "any other catalog failure keeps its own error and the ordinary cadence",
			err:               errors.New("chart catalog object is invalid"),
			wantMessage:       "Failed to update the chart catalog: chart catalog object is invalid",
			wantShortSchedule: false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			decision := Evaluate(Inputs{
				Generation: 1,
				Now:        testNow,
				SyncEnd:    testNow,
				Current:    readyStatus(1),
				Attempted:  true,
				Fetch:      &reposync.FetchOutcome{Charts: 1},
				Catalog:    &reposync.CatalogOutcome{Err: test.err},
			})

			if !errors.Is(decision.Err, test.err) {
				t.Fatalf("decision error = %v, want the catalog failure handed to the work queue", decision.Err)
			}

			synced := apimeta.FindStatusCondition(decision.Status.Conditions, helmv1alpha1.ConditionTypeSynced)
			if synced == nil || synced.Status != metav1.ConditionFalse ||
				synced.Reason != helmv1alpha1.ReasonCatalogUpdateFailed {
				t.Fatalf("synced = %+v, want False with %s", synced, helmv1alpha1.ReasonCatalogUpdateFailed)
			}

			if stalled := apimeta.FindStatusCondition(decision.Status.Conditions, helmv1alpha1.ConditionTypeStalled); stalled != nil {
				t.Fatalf("stalled = %+v, want absent: a catalog write failure is retried, never terminal", stalled)
			}

			if synced.Message != test.wantMessage {
				t.Fatalf("synced message = %q, want %q", synced.Message, test.wantMessage)
			}

			if decision.Status.NextSyncTime == nil {
				t.Fatal("a pass that attempted a synchronization must schedule the next one")
			}

			short := decision.Status.NextSyncTime.Sub(testNow) <= IncompleteSyncDelay+withJitter(IncompleteSyncDelay, SyncBackoffJitter)
			if short != test.wantShortSchedule {
				t.Fatalf("nextSyncTime = %v after the pass, want the short retry: %v",
					decision.Status.NextSyncTime.Sub(testNow), test.wantShortSchedule)
			}
		})
	}
}

func TestEvaluateRetriesASpecChangeThatMadeNoAttempt(t *testing.T) {
	failure := errors.New("etcdserver: request timed out")

	cases := []struct {
		name string
		in   Inputs
	}{
		{name: "auxiliary secrets", in: Inputs{SecretsErr: failure}},
		{name: "internal repository", in: Inputs{InternalRepositoryErr: failure}},
		{name: "catalog rename", in: Inputs{MigrateErr: failure}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := tc.in
			in.Generation = 2
			in.Now = testNow
			in.Current = readyStatus(1)
			in.Current.NextSyncTime = &metav1.Time{Time: testNow.Add(time.Hour)}

			decision := Evaluate(in)

			if decision.Status.NextSyncTime != nil {
				t.Fatalf("nextSyncTime = %v, want none: the new generation was never attempted",
					decision.Status.NextSyncTime)
			}
			if !ShouldAttempt(decision.Status, in.Generation, testNow, false) {
				t.Fatal("the next pass must attempt the generation this one could not")
			}
			if !errors.Is(decision.Err, failure) {
				t.Fatalf("Decision.Err is %v, want %v to hand the retry to the work queue backoff",
					decision.Err, failure)
			}
		})
	}
}
