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

package v1alpha1

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestHelmApplicationRepositoryGetConditions guards the contract the status manager
// relies on: GetConditions must hand out a pointer into the status, not a copy, so
// appending through it is visible on the object.
func TestHelmApplicationRepositoryGetConditions(t *testing.T) {
	repo := &HelmApplicationRepository{}

	conditions := repo.GetConditions()
	*conditions = append(*conditions, metav1.Condition{Type: ConditionTypeReady, Status: metav1.ConditionTrue})

	if len(repo.Status.Conditions) != 1 {
		t.Fatalf("appending through GetConditions did not reach the status: got %d conditions, want 1", len(repo.Status.Conditions))
	}
}

func TestHelmApplicationRepositoryObservedGeneration(t *testing.T) {
	repo := &HelmApplicationRepository{}

	repo.SetObservedGeneration(7)

	if got := repo.GetObservedGeneration(); got != 7 {
		t.Fatalf("GetObservedGeneration() = %d, want 7", got)
	}
	if repo.Status.ObservedGeneration != 7 {
		t.Fatalf("status.observedGeneration = %d, want 7", repo.Status.ObservedGeneration)
	}
}

func TestHelmApplicationRepositoryGetConditionTypesForUpdate(t *testing.T) {
	repo := &HelmApplicationRepository{}

	got := repo.GetConditionTypesForUpdate()

	if len(got) != 1 || got[0] != ConditionTypeReady {
		t.Fatalf("GetConditionTypesForUpdate() = %v, want [%s]", got, ConditionTypeReady)
	}
}

func TestHelmApplicationRepositoryForceReconcileRequired(t *testing.T) {
	cases := []struct {
		name        string
		annotations map[string]string
		want        bool
	}{
		{
			name:        "no annotations at all",
			annotations: nil,
			want:        false,
		},
		{
			name:        "an unrelated annotation",
			annotations: map[string]string{"example.com/other": ""},
			want:        false,
		},
		{
			name:        "the force reconcile annotation with an empty value",
			annotations: map[string]string{AnnotationForceReconcile: ""},
			want:        true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := &HelmApplicationRepository{
				ObjectMeta: metav1.ObjectMeta{Annotations: tc.annotations},
			}

			if got := repo.ForceReconcileRequired(); got != tc.want {
				t.Fatalf("ForceReconcileRequired() = %v, want %v", got, tc.want)
			}
		})
	}
}
