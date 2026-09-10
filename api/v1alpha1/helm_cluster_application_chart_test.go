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

func TestHelmClusterApplicationChartGetConditions(t *testing.T) {
	chart := &HelmClusterApplicationChart{}

	conditions := chart.GetConditions()
	*conditions = append(*conditions, metav1.Condition{Type: ConditionTypeReady, Status: metav1.ConditionTrue})

	if len(chart.Status.Conditions) != 1 {
		t.Fatalf("appending through GetConditions did not reach the status: got %d conditions, want 1", len(chart.Status.Conditions))
	}
}

func TestHelmClusterApplicationChartObservedGeneration(t *testing.T) {
	chart := &HelmClusterApplicationChart{}

	chart.SetObservedGeneration(9)

	if got := chart.GetObservedGeneration(); got != 9 {
		t.Fatalf("GetObservedGeneration() = %d, want 9", got)
	}
}

func TestHelmClusterApplicationChartGetConditionTypesForUpdate(t *testing.T) {
	chart := &HelmClusterApplicationChart{}

	got := chart.GetConditionTypesForUpdate()

	if len(got) != 1 || got[0] != ConditionTypeReady {
		t.Fatalf("GetConditionTypesForUpdate() = %v, want [%s]", got, ConditionTypeReady)
	}
}

// TestChartCatalogStatusIsShared pins that both chart catalog kinds are built on
// one status type. If someone later splits them into per-kind copies, the two
// schemas start drifting apart silently; this assignment stops compiling instead.
func TestChartCatalogStatusIsShared(t *testing.T) {
	namespaced := &HelmApplicationChart{}
	cluster := &HelmClusterApplicationChart{}

	namespaced.Status = cluster.Status

	if namespaced.Status.IconURL != "" {
		t.Fatalf("unexpected iconURL after the assignment: %q", namespaced.Status.IconURL)
	}
}
