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
	"errors"
	"testing"
	"time"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
	"github.com/deckhouse/operator-helm/internal/services"
)

var testNow = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

// readyStatus builds a status that already carries proven Ready for the given generation.
func readyStatus(generation int64) helmv1alpha1.HelmClusterAddonRepositoryStatus {
	return helmv1alpha1.HelmClusterAddonRepositoryStatus{
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
func stalledStatus(generation, staleGeneration int64, reason string) helmv1alpha1.HelmClusterAddonRepositoryStatus {
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

func conditionOf(t *testing.T, status helmv1alpha1.HelmClusterAddonRepositoryStatus, conditionType string) *metav1.Condition {
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
				InternalRepository: services.InternalRepositoryState{Present: true, Ready: true},
				Attempted:          true,
				Fetch:              &services.FetchOutcome{},
				Catalog:            &services.CatalogOutcome{},
			},
			wantReady: metav1.ConditionTrue, wantReadyReason: helmv1alpha1.ReasonSuccess,
			wantSynced: metav1.ConditionTrue,
		},
		{
			name: "awaiting first sync on a fresh object",
			in: Inputs{
				Generation: 1, Now: testNow,
				InternalRepository: services.InternalRepositoryState{Present: true, Ready: true},
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
				InternalRepository: services.InternalRepositoryState{
					Present: true, Ready: false,
					Reason: "FetchFailed", Message: "failed to fetch index",
				},
				Attempted: true,
				Fetch:     &services.FetchOutcome{},
				Catalog:   &services.CatalogOutcome{},
			},
			wantReady: metav1.ConditionFalse, wantReadyReason: "FetchFailed",
			wantSynced:      metav1.ConditionTrue,
			wantReconciling: helmv1alpha1.ReasonReconciling,
		},
		{
			name: "internal repository stalled",
			in: Inputs{
				Generation: 1, Now: testNow, Current: readyStatus(1),
				InternalRepository: services.InternalRepositoryState{
					Present: true, Ready: false, Stalled: true,
					Reason: "InvalidSecretRef", Message: "secret not found",
				},
				Attempted: true,
				Fetch:     &services.FetchOutcome{},
				Catalog:   &services.CatalogOutcome{},
			},
			wantReady: metav1.ConditionFalse, wantReadyReason: "InvalidSecretRef",
			wantSynced:  metav1.ConditionTrue,
			wantStalled: "InvalidSecretRef",
		},
		{
			name: "transient fetch failure keeps Ready latched",
			in: Inputs{
				Generation: 1, Now: testNow, Current: readyStatus(1),
				InternalRepository: services.InternalRepositoryState{Present: true, Ready: true},
				Attempted:          true,
				Fetch:              &services.FetchOutcome{Err: fetchErr, Message: "cannot read index.yaml"},
			},
			wantReady: metav1.ConditionTrue, wantReadyReason: helmv1alpha1.ReasonSuccess,
			wantSynced:      metav1.ConditionFalse,
			wantReconciling: helmv1alpha1.ReasonProgressingWithRetry,
		},
		{
			name: "transient fetch failure without evidence",
			in: Inputs{
				Generation: 1, Now: testNow,
				InternalRepository: services.InternalRepositoryState{Present: true, Ready: true},
				Attempted:          true,
				Fetch:              &services.FetchOutcome{Err: fetchErr, Message: "cannot read index.yaml"},
			},
			wantReady: metav1.ConditionUnknown, wantReadyReason: helmv1alpha1.ReasonAwaitingInitialSync,
			wantSynced:      metav1.ConditionFalse,
			wantReconciling: helmv1alpha1.ReasonProgressingWithRetry,
		},
		{
			name: "terminal fetch failure",
			in: Inputs{
				Generation: 1, Now: testNow, Current: readyStatus(1),
				InternalRepository: services.InternalRepositoryState{Present: true, Ready: true},
				Attempted:          true,
				Fetch: &services.FetchOutcome{
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
			name: "unsupported repository type",
			in: Inputs{
				Generation: 1, Now: testNow, Current: readyStatus(1),
				ConfigErr: &services.ConfigOutcome{
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
				InternalRepository: services.InternalRepositoryState{Present: true, Ready: true},
				Attempted:          true,
				Fetch:              &services.FetchOutcome{},
				Catalog:            &services.CatalogOutcome{Err: writeErr},
			},
			wantReady: metav1.ConditionTrue, wantReadyReason: helmv1alpha1.ReasonSuccess,
			wantSynced:      metav1.ConditionFalse,
			wantReconciling: helmv1alpha1.ReasonProgressingWithRetry,
		},
		{
			name: "generation bump voids the latch",
			in: Inputs{
				Generation: 2, Now: testNow, Current: readyStatus(1),
				InternalRepository: services.InternalRepositoryState{Present: true, Ready: true},
				Attempted:          true,
				Fetch:              &services.FetchOutcome{Err: fetchErr, Message: "cannot read index.yaml"},
			},
			wantReady: metav1.ConditionUnknown, wantReadyReason: helmv1alpha1.ReasonAwaitingInitialSync,
			wantSynced:      metav1.ConditionFalse,
			wantReconciling: helmv1alpha1.ReasonProgressingWithRetry,
		},
		{
			name: "oci repository has no internal object",
			in: Inputs{
				Generation: 1, Now: testNow, Current: readyStatus(1),
				InternalRepository: services.InternalRepositoryState{Present: false},
				Attempted:          true,
				Fetch:              &services.FetchOutcome{},
				Catalog:            &services.CatalogOutcome{},
			},
			wantReady: metav1.ConditionTrue, wantReadyReason: helmv1alpha1.ReasonSuccess,
			wantSynced: metav1.ConditionTrue,
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
				InternalRepository: services.InternalRepositoryState{Present: true, Ready: true},
				Attempted:          true,
				Fetch:              &services.FetchOutcome{},
				Catalog:            &services.CatalogOutcome{Err: catalogWriteErr},
			},
			wantErr: catalogWriteErr,
		},
		{
			name: "repository read failure never reaches Decision.Err",
			in: Inputs{
				Generation: 1, Now: testNow, Current: readyStatus(1),
				InternalRepository: services.InternalRepositoryState{Present: true, Ready: true},
				Attempted:          true,
				Fetch:              &services.FetchOutcome{Err: fetchErr, Message: "cannot read index.yaml"},
			},
			wantErr: nil,
		},
		{
			name: "configuration failure never reaches Decision.Err",
			in: Inputs{
				Generation: 1, Now: testNow, Current: readyStatus(1),
				ConfigErr: &services.ConfigOutcome{
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

			if got.Err != tc.wantErr {
				t.Fatalf("Decision.Err is %v, want %v", got.Err, tc.wantErr)
			}
		})
	}
}

func assertAbnormal(t *testing.T, status helmv1alpha1.HelmClusterAddonRepositoryStatus, conditionType, wantReason string) {
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
