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

package helmclusteraddon

import (
	"context"
	"reflect"
	"sort"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/deckhouse/operator-helm/api/naming"
	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
	"github.com/deckhouse/operator-helm/internal/index"
)

func newChartMapperClient(t *testing.T, objects ...client.Object) client.Client {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("registering client-go scheme: %v", err)
	}
	if err := helmv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("registering helm scheme: %v", err)
	}

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
		// The same function the manager registers, so the repository mapper's lookup
		// is the one it performs in the cluster.
		WithIndex(&helmv1alpha1.HelmClusterAddon{}, index.AddonRepository, index.AddonRepositoryIndexer).
		Build()
}

func chartObject(repoName, chartName string) *helmv1alpha1.HelmClusterAddonChart {
	return &helmv1alpha1.HelmClusterAddonChart{
		ObjectMeta: metav1.ObjectMeta{
			Name: naming.HelmClusterAddonChartName(repoName, chartName),
			Labels: map[string]string{
				helmv1alpha1.LabelRepositoryName: repoName,
				helmv1alpha1.LabelChartName:      chartName,
			},
		},
	}
}

func addonUsingChart(repoName, chartName, version string) *helmv1alpha1.HelmClusterAddon {
	return &helmv1alpha1.HelmClusterAddon{
		ObjectMeta: metav1.ObjectMeta{Name: "consumer"},
		Spec: helmv1alpha1.HelmClusterAddonSpec{
			Namespace: "app",
			Chart: helmv1alpha1.HelmClusterAddonChartRef{
				HelmClusterAddonRepository: repoName,
				HelmClusterAddonChartName:  chartName,
				Version:                    version,
			},
		},
	}
}

// TestmapChartToAddonsEnqueuesTheClaimingAddon covers the reason this watch exists:
// after a terminal probe verdict the addon has no internal HelmChart or OCIRepository
// left for any other watch to catch, so a status change on the chart itself must be
// the thing that wakes it.

func TestMapChartToAddonsEnqueuesTheClaimingAddon(t *testing.T) {
	chart := chartObject("repo-a", "podinfo")
	addon := addonUsingChart("repo-a", "podinfo", "6.7.1")

	c := newChartMapperClient(t, chart, addon)

	requests := mapChartToAddons(c)(context.Background(), chart)

	if len(requests) != 1 {
		t.Fatalf("requests = %+v, want exactly one", requests)
	}
	if requests[0].Name != addon.Name {
		t.Fatalf("request name = %q, want %q", requests[0].Name, addon.Name)
	}
}

// TestMapRepositoryToAddonsEnqueuesOnlyItsOwnConsumers pins the other hand-written
// watch of this controller: a repository write must wake the addons installing from
// that repository, whatever chart of it they use, and no one else. The addons are
// cluster-scoped, so the request carries a name and no namespace.
func TestMapRepositoryToAddonsEnqueuesOnlyItsOwnConsumers(t *testing.T) {
	consumer := addonUsingChart("repo-a", "podinfo", "6.7.1")

	other := addonUsingChart("repo-a", "redis", "1.0.0")
	other.Name = "other-chart-same-repo"

	stranger := addonUsingChart("repo-b", "podinfo", "6.7.1")
	stranger.Name = "another-repo"

	c := newChartMapperClient(t, consumer, other, stranger)

	repository := &helmv1alpha1.HelmClusterAddonRepository{ObjectMeta: metav1.ObjectMeta{Name: "repo-a"}}

	requests := mapRepositoryToAddons(c)(context.Background(), repository)

	got := make([]string, 0, len(requests))
	for _, request := range requests {
		if request.Namespace != "" {
			t.Fatalf("request %+v carries a namespace, the addons are cluster-scoped", request)
		}
		got = append(got, request.Name)
	}
	sort.Strings(got)

	want := []string{"consumer", "other-chart-same-repo"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("enqueued %v, want %v", got, want)
	}
}

// TestMapRepositoryToAddonsIgnoresAnUnrelatedRepository pins the empty case: a
// repository nothing installs from must not enqueue the addons of other
// repositories, which an index value of the wrong shape would do.
func TestMapRepositoryToAddonsIgnoresAnUnrelatedRepository(t *testing.T) {
	c := newChartMapperClient(t, addonUsingChart("repo-a", "podinfo", "6.7.1"))

	repository := &helmv1alpha1.HelmClusterAddonRepository{ObjectMeta: metav1.ObjectMeta{Name: "repo-b"}}

	if requests := mapRepositoryToAddons(c)(context.Background(), repository); len(requests) != 0 {
		t.Fatalf("requests = %+v, want none", requests)
	}
}

func TestMapChartToAddonsNoAddonClaimsTheChart(t *testing.T) {
	chart := chartObject("repo-a", "podinfo")

	c := newChartMapperClient(t, chart)

	if requests := mapChartToAddons(c)(context.Background(), chart); len(requests) != 0 {
		t.Fatalf("requests = %+v, want none", requests)
	}
}

// TestmapChartToAddonsMissingLabels covers a chart object with no repository or chart
// label: the catalog synchronization treats that the same way (fail open, log and move
// on), and this map function must not panic or list every addon by an empty index
// value.
func TestMapChartToAddonsMissingLabels(t *testing.T) {
	chart := &helmv1alpha1.HelmClusterAddonChart{
		ObjectMeta: metav1.ObjectMeta{Name: "orphan-chart"},
	}
	addon := addonUsingChart("repo-a", "podinfo", "6.7.1")

	c := newChartMapperClient(t, chart, addon)

	if requests := mapChartToAddons(c)(context.Background(), chart); len(requests) != 0 {
		t.Fatalf("requests = %+v, want none for a chart with no labels", requests)
	}
}
