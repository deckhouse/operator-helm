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

package utils

import (
	"context"
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

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

// TestMapChartToAddonsEnqueuesTheClaimingAddon covers the reason this watch exists:
// after a terminal probe verdict the addon has no internal HelmChart or OCIRepository
// left for any other watch to catch, so a status change on the chart itself must be
// the thing that wakes it.
func TestMapChartToAddonsEnqueuesTheClaimingAddon(t *testing.T) {
	chart := chartObject("repo-a", "podinfo")
	addon := addonUsingChart("repo-a", "podinfo", "6.7.1")

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
	chart := chartObject("repo-a", "podinfo")

	c := newChartMapperClient(t, chart)

	if requests := MapChartToAddons(c)(context.Background(), chart); len(requests) != 0 {
		t.Fatalf("requests = %+v, want none", requests)
	}
}

// TestMapChartToAddonsMissingLabels covers a chart object with no repository or chart
// label: the catalog synchronization treats that the same way (fail open, log and move
// on), and this map function must not panic or list every addon by an empty index
// value.
func TestMapChartToAddonsMissingLabels(t *testing.T) {
	chart := &helmv1alpha1.HelmClusterAddonChart{
		ObjectMeta: metav1.ObjectMeta{Name: "orphan-chart"},
	}
	addon := addonUsingChart("repo-a", "podinfo", "6.7.1")

	c := newChartMapperClient(t, chart, addon)

	if requests := MapChartToAddons(c)(context.Background(), chart); len(requests) != 0 {
		t.Fatalf("requests = %+v, want none for a chart with no labels", requests)
	}
}

func TestMapNamespacedInternalResources(t *testing.T) {
	const target = "d8-operator-helm"

	mapper := MapNamespacedInternalResources(
		"test-controller", target,
		helmv1alpha1.LabelManagedBy, helmv1alpha1.LabelManagedByValue,
		helmv1alpha1.HelmApplicationRepositoryLabelSourceName, helmv1alpha1.LabelSourceNamespace,
	)

	secret := func(namespace string, labels map[string]string) *corev1.Secret {
		return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "internal", Namespace: namespace, Labels: labels}}
	}

	full := map[string]string{
		helmv1alpha1.LabelManagedBy:                           helmv1alpha1.LabelManagedByValue,
		helmv1alpha1.HelmApplicationRepositoryLabelSourceName: "stable",
		helmv1alpha1.LabelSourceNamespace:                     "team-a",
	}
	withoutNamespace := map[string]string{
		helmv1alpha1.LabelManagedBy:                           helmv1alpha1.LabelManagedByValue,
		helmv1alpha1.HelmApplicationRepositoryLabelSourceName: "stable",
	}
	withoutName := map[string]string{
		helmv1alpha1.LabelManagedBy:       helmv1alpha1.LabelManagedByValue,
		helmv1alpha1.LabelSourceNamespace: "team-a",
	}
	foreign := map[string]string{
		helmv1alpha1.LabelManagedBy:                           "someone-else",
		helmv1alpha1.HelmApplicationRepositoryLabelSourceName: "stable",
		helmv1alpha1.LabelSourceNamespace:                     "team-a",
	}

	cases := []struct {
		name string
		obj  client.Object
		want []reconcile.Request
	}{
		{
			name: "maps to the namespaced source",
			obj:  secret(target, full),
			want: []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: "team-a", Name: "stable"}}},
		},
		{name: "ignores objects outside the target namespace", obj: secret("team-a", full)},
		{name: "ignores objects managed by someone else", obj: secret(target, foreign)},
		{name: "skips an object without the source name", obj: secret(target, withoutName)},
		{name: "skips an object without the source namespace", obj: secret(target, withoutNamespace)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := mapper(context.Background(), tc.obj)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("requests = %v, want %v", got, tc.want)
			}
		})
	}
}
