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
	"context"
	"testing"

	"github.com/fluxcd/pkg/apis/meta"
	sourcev1 "github.com/fluxcd/source-controller/api/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/deckhouse/operator-helm/api/naming"
	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
	"github.com/deckhouse/operator-helm/internal/adapter"
	"github.com/deckhouse/operator-helm/internal/utils"
)

func newCharts(t *testing.T, objects ...client.Object) (*Charts, client.Client) {
	t.Helper()

	scheme := testScheme(t)
	if err := sourcev1.AddToScheme(scheme); err != nil {
		t.Fatalf("registering source scheme: %v", err)
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()

	return NewCharts(c, testNamespace), c
}

func chartTestRepository() *helmv1alpha1.HelmClusterAddonRepository {
	return &helmv1alpha1.HelmClusterAddonRepository{
		ObjectMeta: metav1.ObjectMeta{Name: "example"},
		Spec:       helmv1alpha1.RepositorySpec{URL: "https://charts.example.invalid/stable"},
	}
}

func TestChartsEnsureForcesReconcileFromAddon(t *testing.T) {
	addon := testAddon()
	addon.Annotations = map[string]string{helmv1alpha1.AnnotationForceReconcile: "2026-01-01T00:00:00Z"}
	service, c := newCharts(t, addon)

	service.Ensure(context.Background(), adapter.NewAddonRelease(addon), adapter.NewAddonRepository(chartTestRepository()))

	chart := &sourcev1.HelmChart{}
	key := client.ObjectKey{Name: utils.GetInternalHelmChartName(addon.Name), Namespace: testNamespace}
	if err := c.Get(context.Background(), key, chart); err != nil {
		t.Fatalf("helm chart was not created: %v", err)
	}

	if chart.Annotations[meta.ReconcileRequestAnnotation] == "" {
		t.Errorf("%s must be stamped on the helm chart", meta.ReconcileRequestAnnotation)
	}
	if chart.Annotations[meta.ForceRequestAnnotation] == "" {
		t.Errorf("%s must be stamped on the helm chart", meta.ForceRequestAnnotation)
	}
}

func TestChartsEnsureDoesNotForceReconcileWithoutAnnotation(t *testing.T) {
	addon := testAddon()
	service, c := newCharts(t, addon)

	service.Ensure(context.Background(), adapter.NewAddonRelease(addon), adapter.NewAddonRepository(chartTestRepository()))

	chart := &sourcev1.HelmChart{}
	key := client.ObjectKey{Name: utils.GetInternalHelmChartName(addon.Name), Namespace: testNamespace}
	if err := c.Get(context.Background(), key, chart); err != nil {
		t.Fatalf("helm chart was not created: %v", err)
	}

	if _, found := chart.Annotations[meta.ReconcileRequestAnnotation]; found {
		t.Errorf("%s must not be stamped without a force request", meta.ReconcileRequestAnnotation)
	}
}

func TestChartsEnsureKeepsForeignLabels(t *testing.T) {
	addon := testAddon()
	existing := &sourcev1.HelmChart{
		ObjectMeta: metav1.ObjectMeta{
			Name:      utils.GetInternalHelmChartName(addon.Name),
			Namespace: testNamespace,
			Labels:    map[string]string{"cost-center": "team-a"},
		},
	}
	service, c := newCharts(t, addon, existing)

	rel := adapter.NewAddonRelease(addon)
	service.Ensure(context.Background(), rel, adapter.NewAddonRepository(chartTestRepository()))

	chart := &sourcev1.HelmChart{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(existing), chart); err != nil {
		t.Fatalf("getting helm chart: %v", err)
	}

	if chart.Labels["cost-center"] != "team-a" {
		t.Fatalf("labels = %v, want the foreign label preserved", chart.Labels)
	}
	for key, want := range rel.HelmChartLabels() {
		if chart.Labels[key] != want {
			t.Fatalf("label %q = %q, want %q", key, chart.Labels[key], want)
		}
	}
}

func TestChartsEnsureDropsTheCatalogLabelOfThePreviousRepositoryKind(t *testing.T) {
	app := testApplication()
	app.Spec.Chart.Repository = ""
	app.Spec.Chart.ClusterRepository = "stable"

	rel := adapter.NewApplicationRelease(app)
	stale := naming.HelmApplicationChartName("stable", app.Spec.Chart.Name)

	existing := &sourcev1.HelmChart{
		ObjectMeta: metav1.ObjectMeta{
			Name:      rel.InternalNames().HelmChart,
			Namespace: testNamespace,
			Labels: map[string]string{
				helmv1alpha1.HelmApplicationChartLabelSourceName: stale,
				"cost-center": "team-a",
			},
		},
	}
	service, c := newCharts(t, existing)

	repo := &helmv1alpha1.HelmClusterApplicationRepository{ObjectMeta: metav1.ObjectMeta{Name: "stable"}}
	service.Ensure(context.Background(), rel, adapter.NewClusterApplicationRepository(repo))

	chart := &sourcev1.HelmChart{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(existing), chart); err != nil {
		t.Fatalf("getting helm chart: %v", err)
	}

	if _, found := chart.Labels[helmv1alpha1.HelmApplicationChartLabelSourceName]; found {
		t.Fatalf("labels = %v, want the catalog label of the previous repository kind removed", chart.Labels)
	}
	want := naming.HelmClusterApplicationChartName("stable", app.Spec.Chart.Name)
	if chart.Labels[helmv1alpha1.HelmClusterApplicationChartLabelSourceName] != want {
		t.Fatalf("labels = %v, want the catalog label of the current repository kind %q", chart.Labels, want)
	}
	if chart.Labels["cost-center"] != "team-a" {
		t.Fatalf("labels = %v, want the foreign label preserved", chart.Labels)
	}
}

func TestChartsEnsureGivesTheChartAPassOfItsOwn(t *testing.T) {
	addon := testAddon()
	service, c := newCharts(t, addon)

	service.Ensure(context.Background(), adapter.NewAddonRelease(addon), adapter.NewAddonRepository(chartTestRepository()))

	chart := &sourcev1.HelmChart{}
	key := client.ObjectKey{Name: utils.GetInternalHelmChartName(addon.Name), Namespace: testNamespace}
	if err := c.Get(context.Background(), key, chart); err != nil {
		t.Fatalf("helm chart was not created: %v", err)
	}

	if chart.Spec.Interval.Duration <= 0 {
		t.Fatalf("interval = %v: a chart without one is never reconciled on its own", chart.Spec.Interval.Duration)
	}
	if chart.Spec.Interval.Duration != internalChartInterval {
		t.Fatalf("interval = %v, want %v", chart.Spec.Interval.Duration, internalChartInterval)
	}
}

func TestChartsEnsureRepairsAChartLeftWithoutAnInterval(t *testing.T) {
	addon := testAddon()
	existing := &sourcev1.HelmChart{
		ObjectMeta: metav1.ObjectMeta{
			Name:      utils.GetInternalHelmChartName(addon.Name),
			Namespace: testNamespace,
		},
		Spec: sourcev1.HelmChartSpec{
			Chart:     addon.Spec.Chart.HelmClusterAddonChartName,
			Version:   addon.Spec.Chart.Version,
			SourceRef: sourcev1.LocalHelmChartSourceReference{Kind: sourcev1.HelmRepositoryKind, Name: "whatever"},
		},
	}
	service, c := newCharts(t, addon, existing)

	ctx := context.Background()
	rel, repo := adapter.NewAddonRelease(addon), adapter.NewAddonRepository(chartTestRepository())
	service.Ensure(ctx, rel, repo)

	chart := &sourcev1.HelmChart{}
	key := client.ObjectKeyFromObject(existing)
	if err := c.Get(ctx, key, chart); err != nil {
		t.Fatalf("getting helm chart: %v", err)
	}
	if chart.Spec.Interval.Duration != internalChartInterval {
		t.Fatalf("interval = %v, want the pass to have corrected it to %v",
			chart.Spec.Interval.Duration, internalChartInterval)
	}

	before := chart.ResourceVersion
	service.Ensure(ctx, rel, repo)

	settled := &sourcev1.HelmChart{}
	if err := c.Get(ctx, key, settled); err != nil {
		t.Fatalf("getting helm chart: %v", err)
	}
	if settled.ResourceVersion != before {
		t.Fatalf("the pass after the correction wrote again: resourceVersion %s -> %s",
			before, settled.ResourceVersion)
	}
}
