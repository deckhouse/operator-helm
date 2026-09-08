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

package services

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
	"github.com/deckhouse/operator-helm/internal/index"
)

func newChartMapperClient(t *testing.T, objects ...client.Object) client.Client {
	t.Helper()

	scheme := testScheme(t)

	return fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objects...).
		WithIndex(&helmv1alpha1.HelmClusterAddon{}, index.AddonChart, func(obj client.Object) []string {
			addon := obj.(*helmv1alpha1.HelmClusterAddon)

			return []string{index.AddonChartValue(
				addon.Spec.Chart.HelmClusterAddonRepository,
				addon.Spec.Chart.HelmClusterAddonChartName,
			)}
		}).
		Build()
}

// TestMapChartToAddonsEnqueuesTheClaimingAddon covers the reason this watch exists:
// after a terminal probe verdict the addon has no internal HelmChart or OCIRepository
// left for any other watch to catch, so a status change on the chart itself must be
// the thing that wakes it.
func TestMapChartToAddonsEnqueuesTheClaimingAddon(t *testing.T) {
	chart := existingChart("repo-a", "podinfo")
	addon := addonUsing("repo-a", "podinfo", "6.7.1")

	c := newChartMapperClient(t, chart, addon)

	requests := MapChartToAddons(c)(context.Background(), chart)

	if len(requests) != 1 {
		t.Fatalf("requests = %+v, want exactly one", requests)
	}
	if requests[0].Name != addon.Name {
		t.Fatalf("request name = %q, want %q", requests[0].Name, addon.Name)
	}
}

// TestMapChartToAddonsNoAddonClaimsTheChart covers the case where nothing references
// the chart yet: no request should be produced.
func TestMapChartToAddonsNoAddonClaimsTheChart(t *testing.T) {
	chart := existingChart("repo-a", "podinfo")

	c := newChartMapperClient(t, chart)

	if requests := MapChartToAddons(c)(context.Background(), chart); len(requests) != 0 {
		t.Fatalf("requests = %+v, want none", requests)
	}
}

// TestMapChartToAddonsMissingLabels covers a chart object with no repository or chart
// label: knownCharts treats that the same way (fail open, log and move on), and this
// map function must not panic or list every addon by an empty index value.
func TestMapChartToAddonsMissingLabels(t *testing.T) {
	chart := &helmv1alpha1.HelmClusterAddonChart{
		ObjectMeta: metav1.ObjectMeta{Name: "orphan-chart"},
	}
	addon := addonUsing("repo-a", "podinfo", "6.7.1")

	c := newChartMapperClient(t, chart, addon)

	if requests := MapChartToAddons(c)(context.Background(), chart); len(requests) != 0 {
		t.Fatalf("requests = %+v, want none for a chart with no labels", requests)
	}
}
