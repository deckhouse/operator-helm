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

package adapter

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
	"github.com/deckhouse/operator-helm/internal/index"
)

func addonClient(t *testing.T, objects ...client.Object) client.Client {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := helmv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("registering helm scheme: %v", err)
	}

	return fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objects...).
		WithIndex(&helmv1alpha1.HelmClusterAddon{}, index.AddonChart, func(obj client.Object) []string {
			addon := obj.(*helmv1alpha1.HelmClusterAddon)

			return []string{index.AddonChartValue(addon.Spec.Chart.HelmClusterAddonRepository, addon.Spec.Chart.HelmClusterAddonChartName)}
		}).
		WithIndex(&helmv1alpha1.HelmClusterAddon{}, index.AddonRepository, func(obj client.Object) []string {
			addon := obj.(*helmv1alpha1.HelmClusterAddon)

			return []string{addon.Spec.Chart.HelmClusterAddonRepository}
		}).
		Build()
}

// TestAddonCatalogInUseVersionsCountsDesiredAndLastApplied reproduces the addon
// in-use rule that moved here from services: the desired version and the last
// applied one both count, the latter only while it still names this chart.
func TestAddonCatalogInUseVersionsCountsDesiredAndLastApplied(t *testing.T) {
	addon := &helmv1alpha1.HelmClusterAddon{
		ObjectMeta: metav1.ObjectMeta{Name: "consumer"},
		Spec: helmv1alpha1.HelmClusterAddonSpec{
			Namespace: "app",
			Chart:     helmv1alpha1.HelmClusterAddonChartRef{HelmClusterAddonRepository: "example", HelmClusterAddonChartName: "podinfo", Version: "2.0.0"},
		},
		Status: helmv1alpha1.HelmClusterAddonStatus{
			LastAppliedChart: &helmv1alpha1.HelmClusterAddonLastAppliedChartRef{HelmClusterAddonRepository: "example", HelmClusterAddonChartName: "podinfo", Version: "1.0.0"},
		},
	}
	switched := &helmv1alpha1.HelmClusterAddon{
		ObjectMeta: metav1.ObjectMeta{Name: "switched"},
		Spec: helmv1alpha1.HelmClusterAddonSpec{
			Namespace: "app",
			Chart:     helmv1alpha1.HelmClusterAddonChartRef{HelmClusterAddonRepository: "example", HelmClusterAddonChartName: "nginx", Version: "3.0.0"},
		},
		Status: helmv1alpha1.HelmClusterAddonStatus{
			// Last applied still names podinfo, but Spec moved to nginx: podinfo's
			// index entry no longer lists this addon, so 9.9.9 must not be protected.
			LastAppliedChart: &helmv1alpha1.HelmClusterAddonLastAppliedChartRef{HelmClusterAddonRepository: "example", HelmClusterAddonChartName: "podinfo", Version: "9.9.9"},
		},
	}

	c := addonClient(t, addon, switched)
	cat := NewAddonCatalog(c)
	repo := NewAddonRepository(&helmv1alpha1.HelmClusterAddonRepository{ObjectMeta: metav1.ObjectMeta{Name: "example"}})

	inUse, err := cat.InUseVersions(context.Background(), repo, "podinfo")
	if err != nil {
		t.Fatalf("InUseVersions returned %v", err)
	}

	for _, want := range []string{"1.0.0", "2.0.0"} {
		if _, ok := inUse[want]; !ok {
			t.Fatalf("in use = %v, want %s included", inUse, want)
		}
	}
	if _, ok := inUse["9.9.9"]; ok {
		t.Fatalf("in use = %v, a stale lastAppliedChart of an addon that moved to another chart must not count", inUse)
	}
	if len(inUse) != 2 {
		t.Fatalf("in use = %v, want exactly two versions", inUse)
	}
}

// TestApplicationCatalogInUseVersionsSeesOnlyItsOwnNamespace pins spec 6.2: the
// in-use versions of a namespaced repository's chart come from the applications of
// that namespace, or a same-named repository elsewhere would keep versions alive.
func TestApplicationCatalogInUseVersionsSeesOnlyItsOwnNamespace(t *testing.T) {
	inA := namespacedApp("team-a", "a1", "stable")
	inB := namespacedApp("team-b", "b1", "stable")
	inB.Spec.Chart.Version = "9.9.9"

	c := applicationClient(t, inA, inB)
	cat := NewApplicationCatalog(c)
	repo := NewApplicationRepository(&helmv1alpha1.HelmApplicationRepository{ObjectMeta: metav1.ObjectMeta{Name: "stable", Namespace: "team-a"}})

	inUse, err := cat.InUseVersions(context.Background(), repo, "podinfo")
	if err != nil {
		t.Fatalf("InUseVersions returned %v", err)
	}
	if _, ok := inUse["6.7.1"]; !ok || len(inUse) != 1 {
		t.Fatalf("in use = %v, want only team-a's 6.7.1", inUse)
	}
}
