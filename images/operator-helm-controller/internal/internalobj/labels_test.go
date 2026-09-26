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

package internalobj

import (
	"maps"
	"testing"

	sourcev1 "github.com/fluxcd/source-controller/api/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
)

// TestApplyOwnedLabelsReconcilesOnlyTheControllersOwnLabels pins the single policy
// every internal object follows: a label nobody here owns survives untouched, while a
// label qualified with our API group is brought to the desired set — added, updated
// or, when it is no longer wanted, removed.
func TestApplyOwnedLabelsReconcilesOnlyTheControllersOwnLabels(t *testing.T) {
	tests := []struct {
		name     string
		existing map[string]string
		desired  map[string]string
		want     map[string]string
	}{
		{
			name:    "an object without labels gets the desired set",
			desired: map[string]string{helmv1alpha1.LabelManagedBy: helmv1alpha1.LabelManagedByValue},
			want:    map[string]string{helmv1alpha1.LabelManagedBy: helmv1alpha1.LabelManagedByValue},
		},
		{
			name:     "a label set by someone else is kept",
			existing: map[string]string{"cost-center": "team-a", "policy.example.com/tier": "gold"},
			desired:  map[string]string{helmv1alpha1.LabelManagedBy: helmv1alpha1.LabelManagedByValue},
			want: map[string]string{
				"cost-center":               "team-a",
				"policy.example.com/tier":   "gold",
				helmv1alpha1.LabelManagedBy: helmv1alpha1.LabelManagedByValue,
			},
		},
		{
			name: "an owned label that is no longer desired is dropped",
			existing: map[string]string{
				helmv1alpha1.LabelManagedBy:                             helmv1alpha1.LabelManagedByValue,
				helmv1alpha1.HelmClusterApplicationChartLabelSourceName: "stale",
			},
			desired: map[string]string{
				helmv1alpha1.LabelManagedBy:                      helmv1alpha1.LabelManagedByValue,
				helmv1alpha1.HelmApplicationChartLabelSourceName: "current",
			},
			want: map[string]string{
				helmv1alpha1.LabelManagedBy:                      helmv1alpha1.LabelManagedByValue,
				helmv1alpha1.HelmApplicationChartLabelSourceName: "current",
			},
		},
		{
			name:     "an owned label with a stale value is overwritten",
			existing: map[string]string{helmv1alpha1.LabelSourceNamespace: "old"},
			desired:  map[string]string{helmv1alpha1.LabelSourceNamespace: "new"},
			want:     map[string]string{helmv1alpha1.LabelSourceNamespace: "new"},
		},
		{
			name:     "an unqualified label is never treated as ours",
			existing: map[string]string{helmv1alpha1.LabelDeckhouseHeritage: helmv1alpha1.LabelDeckhouseHeritageValue},
			desired:  map[string]string{helmv1alpha1.LabelManagedBy: helmv1alpha1.LabelManagedByValue},
			want: map[string]string{
				helmv1alpha1.LabelDeckhouseHeritage: helmv1alpha1.LabelDeckhouseHeritageValue,
				helmv1alpha1.LabelManagedBy:         helmv1alpha1.LabelManagedByValue,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			obj := &sourcev1.HelmChart{ObjectMeta: metav1.ObjectMeta{Labels: tt.existing}}

			applyOwnedLabels(obj, tt.desired)

			if !maps.Equal(obj.Labels, tt.want) {
				t.Fatalf("labels = %v, want %v", obj.Labels, tt.want)
			}
		})
	}
}
