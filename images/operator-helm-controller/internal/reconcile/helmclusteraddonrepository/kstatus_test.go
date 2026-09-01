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

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/cli-utils/pkg/kstatus/status"

	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
	"github.com/deckhouse/operator-helm/internal/services"
)

// computeStatus renders the decision as the repository object would look in the
// cluster and asks the real kstatus library for its verdict.
func computeStatus(t *testing.T, generation int64, decision Decision) status.Status {
	t.Helper()

	repo := &helmv1alpha1.HelmClusterAddonRepository{}
	repo.SetGroupVersionKind(helmv1alpha1.HelmClusterAddonRepositoryGVK)
	repo.SetName("test-repo")
	repo.SetGeneration(generation)
	repo.Status = decision.Status

	content, err := runtime.DefaultUnstructuredConverter.ToUnstructured(repo)
	if err != nil {
		t.Fatalf("converting repository to unstructured: %v", err)
	}

	result, err := status.Compute(&unstructured.Unstructured{Object: content})
	if err != nil {
		t.Fatalf("computing kstatus: %v", err)
	}

	return result.Status
}

func TestKstatusVerdicts(t *testing.T) {
	cases := []struct {
		name string
		in   Inputs
		want status.Status
	}{
		{
			name: "healthy repository is current",
			in: Inputs{
				Generation: 1, Now: testNow, Current: readyStatus(1),
				InternalRepository: services.InternalRepositoryState{Present: true, Ready: true},
				Attempted:          true,
				Fetch:              &services.FetchOutcome{},
				Catalog:            &services.CatalogOutcome{},
			},
			want: status.CurrentStatus,
		},
		{
			name: "retrying repository is in progress",
			in: Inputs{
				Generation: 1, Now: testNow, Current: readyStatus(1),
				InternalRepository: services.InternalRepositoryState{Present: true, Ready: true},
				Attempted:          true,
				Fetch:              &services.FetchOutcome{Err: errors.New("connection refused"), Message: "cannot read index.yaml"},
			},
			want: status.InProgressStatus,
		},
		{
			name: "terminal failure is failed",
			in: Inputs{
				Generation: 1, Now: testNow, Current: readyStatus(1),
				InternalRepository: services.InternalRepositoryState{Present: true, Ready: true},
				Attempted:          true,
				Fetch: &services.FetchOutcome{
					Err: errors.New("unauthorized"), Terminal: true,
					Reason: helmv1alpha1.ReasonAuthenticationFailed, Message: "rejected the credentials",
				},
			},
			want: status.FailedStatus,
		},
		{
			// The work-queue retry after a catalog write failure runs a pass with no
			// attempt. Without the carry-forward in evaluateReconciling the object
			// would carry no abnormal-true condition and kstatus would call a
			// permanently broken catalog write Current.
			name: "catalog write failure stays in progress between attempts",
			in: Inputs{
				Generation: 1, Now: testNow, Current: catalogFailedStatus(1),
				InternalRepository: services.InternalRepositoryState{Present: true, Ready: true},
			},
			want: status.InProgressStatus,
		},
		{
			name: "stalled is not masked by a lagging observedGeneration",
			in: Inputs{
				Generation: 3, Now: testNow, Current: readyStatus(1),
				InternalRepository: services.InternalRepositoryState{Present: true, Ready: true},
				Attempted:          true,
				Fetch: &services.FetchOutcome{
					Err: errors.New("not found"), Terminal: true,
					Reason: helmv1alpha1.ReasonSourceNotFound, Message: "repository not found",
				},
			},
			want: status.FailedStatus,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := computeStatus(t, tc.in.Generation, Evaluate(tc.in))
			if got != tc.want {
				t.Fatalf("kstatus verdict is %s, want %s", got, tc.want)
			}
		})
	}
}
