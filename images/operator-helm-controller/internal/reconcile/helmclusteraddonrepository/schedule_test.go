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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
	"github.com/deckhouse/operator-helm/internal/services"
)

func TestBackoffProgression(t *testing.T) {
	fetchErr := errors.New("connection refused")

	cases := []struct {
		name         string
		failuresIn   int32
		wantFailures int32
		wantRequeue  time.Duration
	}{
		{name: "first failure", failuresIn: 0, wantFailures: 1, wantRequeue: 5 * time.Minute},
		{name: "second failure", failuresIn: 1, wantFailures: 2, wantRequeue: 10 * time.Minute},
		{name: "third failure", failuresIn: 2, wantFailures: 3, wantRequeue: 20 * time.Minute},
		{name: "fourth failure", failuresIn: 3, wantFailures: 4, wantRequeue: 40 * time.Minute},
		{name: "fifth failure caps", failuresIn: 4, wantFailures: 5, wantRequeue: time.Hour},
		{name: "beyond cap stays capped", failuresIn: 5, wantFailures: 5, wantRequeue: time.Hour},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Evaluate(Inputs{
				Generation: 1,
				Now:        testNow,
				Current: helmv1alpha1.HelmClusterAddonRepositoryStatus{
					ObservedGeneration:       1,
					ConsecutiveFetchFailures: tc.failuresIn,
				},
				InternalRepository: services.InternalRepositoryState{Present: true, Ready: true},
				Attempted:          true,
				Fetch:              &services.FetchOutcome{Err: fetchErr, Message: "cannot read index.yaml"},
			})

			if got.Status.ConsecutiveFetchFailures != tc.wantFailures {
				t.Fatalf("failures are %d, want %d", got.Status.ConsecutiveFetchFailures, tc.wantFailures)
			}
			if got.RequeueAfter != tc.wantRequeue {
				t.Fatalf("requeue after %s, want %s", got.RequeueAfter, tc.wantRequeue)
			}
			if got.Status.NextSyncTime == nil {
				t.Fatal("nextSyncTime must be set after an attempt")
			}
			if !got.Status.NextSyncTime.Time.Equal(testNow.Add(tc.wantRequeue)) {
				t.Fatalf("nextSyncTime is %s, want %s", got.Status.NextSyncTime.Time, testNow.Add(tc.wantRequeue))
			}
		})
	}
}

func TestSuccessResetsCounterAndRecordsSyncTime(t *testing.T) {
	got := Evaluate(Inputs{
		Generation: 1,
		Now:        testNow,
		Current: helmv1alpha1.HelmClusterAddonRepositoryStatus{
			ObservedGeneration:       1,
			ConsecutiveFetchFailures: 3,
		},
		InternalRepository: services.InternalRepositoryState{Present: true, Ready: true},
		Attempted:          true,
		Fetch:              &services.FetchOutcome{},
		Catalog:            &services.CatalogOutcome{},
	})

	if got.Status.ConsecutiveFetchFailures != 0 {
		t.Fatalf("counter must reset on success, got %d", got.Status.ConsecutiveFetchFailures)
	}
	if got.RequeueAfter != SyncInterval {
		t.Fatalf("requeue after %s, want %s", got.RequeueAfter, SyncInterval)
	}
	if got.Status.LastSuccessfulSyncTime == nil || !got.Status.LastSuccessfulSyncTime.Time.Equal(testNow) {
		t.Fatalf("lastSuccessfulSyncTime is %v, want %s", got.Status.LastSuccessfulSyncTime, testNow)
	}
}

func TestCatalogFailureDoesNotRecordSyncTime(t *testing.T) {
	previous := metav1.NewTime(testNow.Add(-time.Hour))

	got := Evaluate(Inputs{
		Generation: 1,
		Now:        testNow,
		Current: helmv1alpha1.HelmClusterAddonRepositoryStatus{
			ObservedGeneration:     1,
			LastSuccessfulSyncTime: &previous,
		},
		InternalRepository: services.InternalRepositoryState{Present: true, Ready: true},
		Attempted:          true,
		Fetch:              &services.FetchOutcome{},
		Catalog:            &services.CatalogOutcome{Err: errors.New("etcdserver: request timed out")},
	})

	if !got.Status.LastSuccessfulSyncTime.Time.Equal(previous.Time) {
		t.Fatalf("lastSuccessfulSyncTime must not advance on a catalog failure, got %s", got.Status.LastSuccessfulSyncTime.Time)
	}
	if got.Status.ConsecutiveFetchFailures != 0 {
		t.Fatalf("a catalog failure must not count as a fetch failure, got %d", got.Status.ConsecutiveFetchFailures)
	}
	if got.Err == nil {
		t.Fatal("a catalog failure must be returned for the work queue")
	}
}

func TestTerminalFetchSaturatesCounter(t *testing.T) {
	got := Evaluate(Inputs{
		Generation:         1,
		Now:                testNow,
		Current:            helmv1alpha1.HelmClusterAddonRepositoryStatus{ObservedGeneration: 1},
		InternalRepository: services.InternalRepositoryState{Present: true, Ready: true},
		Attempted:          true,
		Fetch: &services.FetchOutcome{
			Err: errors.New("unauthorized"), Terminal: true,
			Reason: helmv1alpha1.ReasonAuthenticationFailed, Message: "rejected the credentials",
		},
	})

	if got.Status.ConsecutiveFetchFailures != MaxFetchFailures {
		t.Fatalf("terminal failure must saturate the counter, got %d", got.Status.ConsecutiveFetchFailures)
	}
	if got.RequeueAfter != MaxSyncBackoff {
		t.Fatalf("requeue after %s, want %s", got.RequeueAfter, MaxSyncBackoff)
	}
}

func TestConfigErrorDoesNotRequeue(t *testing.T) {
	got := Evaluate(Inputs{
		Generation: 1,
		Now:        testNow,
		Current:    helmv1alpha1.HelmClusterAddonRepositoryStatus{ObservedGeneration: 1},
		ConfigErr: &services.ConfigOutcome{
			Reason:  helmv1alpha1.ReasonUnsupportedRepositoryType,
			Message: "unsupported repository schema in use: ftp",
		},
	})

	if got.RequeueAfter != 0 {
		t.Fatalf("a spec-only failure must not requeue, got %s", got.RequeueAfter)
	}
}

func TestGenerationBumpResetsCounter(t *testing.T) {
	got := Evaluate(Inputs{
		Generation: 2,
		Now:        testNow,
		Current: helmv1alpha1.HelmClusterAddonRepositoryStatus{
			ObservedGeneration:       1,
			ConsecutiveFetchFailures: 4,
		},
		InternalRepository: services.InternalRepositoryState{Present: true, Ready: true},
		Attempted:          true,
		Fetch:              &services.FetchOutcome{},
		Catalog:            &services.CatalogOutcome{},
	})

	if got.Status.ConsecutiveFetchFailures != 0 {
		t.Fatalf("counter must reset on a spec change, got %d", got.Status.ConsecutiveFetchFailures)
	}
}

func TestPassWithoutAttemptKeepsSchedule(t *testing.T) {
	next := metav1.NewTime(testNow.Add(3 * time.Minute))

	got := Evaluate(Inputs{
		Generation: 1,
		Now:        testNow,
		Current: helmv1alpha1.HelmClusterAddonRepositoryStatus{
			ObservedGeneration:       1,
			NextSyncTime:             &next,
			ConsecutiveFetchFailures: 2,
			Conditions: []metav1.Condition{
				{
					Type:               helmv1alpha1.ConditionTypeReady,
					Status:             metav1.ConditionTrue,
					Reason:             helmv1alpha1.ReasonSuccess,
					ObservedGeneration: 1,
					LastTransitionTime: metav1.NewTime(testNow.Add(-time.Hour)),
				},
			},
		},
		InternalRepository: services.InternalRepositoryState{Present: true, Ready: true},
	})

	if !got.Status.NextSyncTime.Time.Equal(next.Time) {
		t.Fatalf("nextSyncTime must not move without an attempt, got %s", got.Status.NextSyncTime.Time)
	}
	if got.Status.ConsecutiveFetchFailures != 2 {
		t.Fatalf("counter must not move without an attempt, got %d", got.Status.ConsecutiveFetchFailures)
	}
	if got.RequeueAfter != 3*time.Minute {
		t.Fatalf("requeue after %s, want the remaining 3m", got.RequeueAfter)
	}
}

func TestShouldAttempt(t *testing.T) {
	future := metav1.NewTime(testNow.Add(time.Minute))
	past := metav1.NewTime(testNow.Add(-time.Minute))

	cases := []struct {
		name       string
		current    helmv1alpha1.HelmClusterAddonRepositoryStatus
		generation int64
		forced     bool
		want       bool
	}{
		{name: "fresh object", current: helmv1alpha1.HelmClusterAddonRepositoryStatus{}, generation: 1, want: true},
		{
			name:       "schedule not reached",
			current:    helmv1alpha1.HelmClusterAddonRepositoryStatus{ObservedGeneration: 1, NextSyncTime: &future},
			generation: 1,
			want:       false,
		},
		{
			name:       "schedule reached",
			current:    helmv1alpha1.HelmClusterAddonRepositoryStatus{ObservedGeneration: 1, NextSyncTime: &past},
			generation: 1,
			want:       true,
		},
		{
			name:       "forced beats the schedule",
			current:    helmv1alpha1.HelmClusterAddonRepositoryStatus{ObservedGeneration: 1, NextSyncTime: &future},
			generation: 1,
			forced:     true,
			want:       true,
		},
		{
			name:       "spec change beats the schedule",
			current:    helmv1alpha1.HelmClusterAddonRepositoryStatus{ObservedGeneration: 1, NextSyncTime: &future},
			generation: 2,
			want:       true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ShouldAttempt(tc.current, tc.generation, testNow, tc.forced); got != tc.want {
				t.Fatalf("ShouldAttempt = %v, want %v", got, tc.want)
			}
		})
	}
}
