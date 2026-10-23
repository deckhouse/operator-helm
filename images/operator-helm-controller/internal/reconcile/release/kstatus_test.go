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

package release

import (
	"errors"
	"testing"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/cli-utils/pkg/kstatus/status"

	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
	"github.com/deckhouse/operator-helm/internal/internalobj"
)

// computeStatus writes the decision onto a release the way the reconciler does and asks
// kstatus what a consumer of the release would see.
func computeStatus(t *testing.T, in Inputs, decision Decision) status.Status {
	t.Helper()

	rel := &helmv1alpha1.HelmClusterAddon{}
	rel.SetGroupVersionKind(helmv1alpha1.HelmClusterAddonGVK)
	rel.SetName("test-release")
	rel.SetGeneration(in.Generation)
	rel.SetObservedGeneration(in.ObservedGeneration)

	conditions := rel.GetConditions()
	for _, condition := range decision.Conditions {
		apimeta.SetStatusCondition(conditions, condition)
	}

	for _, conditionType := range decision.RemoveConditions {
		apimeta.RemoveStatusCondition(conditions, conditionType)
	}

	if decision.ObservedGeneration != nil {
		rel.SetObservedGeneration(*decision.ObservedGeneration)
	}

	content, err := runtime.DefaultUnstructuredConverter.ToUnstructured(rel)
	if err != nil {
		t.Fatalf("converting release to unstructured: %v", err)
	}

	result, err := status.Compute(&unstructured.Unstructured{Object: content})
	if err != nil {
		t.Fatalf("computing kstatus: %v", err)
	}

	return result.Status
}

func TestKstatusVerdicts(t *testing.T) {
	cases := []struct {
		name    string
		outcome Outcome
		want    status.Status
	}{
		{
			name: "deployed release is current",
			outcome: ChartReached{
				Internal: ServedByHelmChart{Result: internalobj.ChartOutcome{Internal: readyInternal()}},
				Release:  &internalobj.ReleaseOutcome{Internal: readyInternal(), ChartDeployed: true},
			},
			want: status.CurrentStatus,
		},
		{
			name: "a version the catalog does not offer is never current",
			outcome: ChartReached{Internal: SourceUnresolved{Failure: Failure{
				Reason:   helmv1alpha1.ReasonChartVersionNotFound,
				Message:  "Version 6.7.2 of chart podinfo is not in the catalog of repository example",
				Err:      errors.New("chart version not found"),
				BadInput: true,
			}}},
			want: status.InProgressStatus,
		},
		{
			name: "a terminal step failure is failed",
			outcome: StepFailed{Failure{
				Reason:   helmv1alpha1.ReasonFailed,
				Message:  "Target namespace cannot be a system namespace",
				Err:      errors.New("system namespace"),
				Terminal: true,
			}},
			want: status.FailedStatus,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := baseInputs()
			in.ObservedGeneration = 1
			in.Outcome = tc.outcome

			if got := computeStatus(t, in, Evaluate(in)); got != tc.want {
				t.Fatalf("kstatus verdict is %s, want %s", got, tc.want)
			}
		})
	}
}
