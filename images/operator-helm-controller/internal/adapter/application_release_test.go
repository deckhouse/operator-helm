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
	"reflect"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/deckhouse/operator-helm/api/naming"
	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
	"github.com/deckhouse/operator-helm/internal/index"
	"github.com/deckhouse/operator-helm/internal/source"
	"github.com/deckhouse/operator-helm/internal/utils"
)

func applicationClient(t *testing.T, objects ...client.Object) client.Client {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := helmv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("registering helm scheme: %v", err)
	}

	return fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objects...).
		WithIndex(&helmv1alpha1.HelmApplication{}, index.ApplicationRepository, index.ApplicationRepositoryIndexer).
		WithIndex(&helmv1alpha1.HelmApplication{}, index.ApplicationChart, index.ApplicationChartIndexer).
		Build()
}

func namespacedApp(namespace, name, repo string) *helmv1alpha1.HelmApplication {
	return &helmv1alpha1.HelmApplication{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Generation: 1},
		Spec: helmv1alpha1.HelmApplicationSpec{
			Chart: helmv1alpha1.HelmApplicationChartRef{Name: "podinfo", Repository: repo, Version: "6.7.1"},
		},
	}
}

func clusterApp(namespace, name, repo string) *helmv1alpha1.HelmApplication {
	app := namespacedApp(namespace, name, "")
	app.Spec.Chart.ClusterRepository = repo

	return app
}

// TestApplicationReleaseResolvesTheRepositoryReference pins the one place the XOR of
// spec.chart.repository / spec.chart.clusterRepository is read: everything
// downstream sees a kind, a namespace and a name.
func TestApplicationReleaseResolvesTheRepositoryReference(t *testing.T) {
	cases := []struct {
		name          string
		app           *helmv1alpha1.HelmApplication
		wantRef       source.RepositoryRef
		wantChartLbl  string
		wantChartName string
	}{
		{
			name:          "namespaced repository lives in the application namespace",
			app:           namespacedApp("team-a", "my-app", "stable"),
			wantRef:       source.RepositoryRef{Kind: helmv1alpha1.HelmApplicationRepositoryKind, Namespace: "team-a", Name: "stable"},
			wantChartLbl:  helmv1alpha1.HelmApplicationChartLabelSourceName,
			wantChartName: naming.ApplicationChartName("stable", "podinfo"),
		},
		{
			name:          "cluster repository has no namespace",
			app:           clusterApp("team-a", "my-app", "shared"),
			wantRef:       source.RepositoryRef{Kind: helmv1alpha1.HelmClusterApplicationRepositoryKind, Name: "shared"},
			wantChartLbl:  helmv1alpha1.HelmClusterApplicationChartLabelSourceName,
			wantChartName: naming.ClusterApplicationChartName("shared", "podinfo"),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rel := NewApplicationRelease(tc.app)

			want := source.ChartRef{Repository: tc.wantRef, Chart: "podinfo", Version: "6.7.1"}
			if got := rel.ChartRef(); got != want {
				t.Fatalf("ChartRef = %+v, want %+v", got, want)
			}
			if got := rel.HelmChartLabels()[tc.wantChartLbl]; got != tc.wantChartName {
				t.Fatalf("catalog label %s = %q, want %q", tc.wantChartLbl, got, tc.wantChartName)
			}
			if rel.TargetNamespace() != "team-a" {
				t.Fatalf("TargetNamespace = %q, want the application's own namespace", rel.TargetNamespace())
			}
		})
	}
}

func TestApplicationReleaseNamesAndLabels(t *testing.T) {
	rel := NewApplicationRelease(namespacedApp("team-a", "my-app", "stable"))

	if rel.Kind() != helmv1alpha1.HelmApplicationKind {
		t.Fatalf("Kind = %q", rel.Kind())
	}
	if rel.ReleaseName() != "hap-my-app" {
		t.Fatalf("ReleaseName = %q, want hap-<name>", rel.ReleaseName())
	}

	wantLabels := map[string]string{
		helmv1alpha1.LabelManagedBy:                 helmv1alpha1.LabelManagedByValue,
		helmv1alpha1.HelmApplicationLabelSourceName: "my-app",
		helmv1alpha1.LabelSourceNamespace:           "team-a",
	}
	if got := rel.SourceLabels(); !reflect.DeepEqual(got, wantLabels) {
		t.Fatalf("SourceLabels = %v, want %v", got, wantLabels)
	}

	const derived = "hap-team-a-my-app-de20cad3987c"
	want := source.ReleaseNames{HelmChart: derived, HelmRelease: derived, OCIRepository: derived, ServiceAccount: derived}
	if got := rel.InternalNames(); got != want {
		t.Fatalf("InternalNames = %+v, want %+v", got, want)
	}
	if want.HelmChart != utils.DerivedName("hap", helmv1alpha1.HelmApplicationKind, "team-a", "my-app") {
		t.Fatal("the pinned literal drifted from DerivedName")
	}

	other := NewApplicationRelease(namespacedApp("team-b", "my-app", "stable"))
	if other.InternalNames().HelmRelease == rel.InternalNames().HelmRelease {
		t.Fatal("same-named applications in different namespaces must derive different internal names")
	}
}

// TestApplicationReleaseReplacesLastAppliedChartWholesale pins the rule from the API
// design: a merge into lastAppliedChart would leave a stale repository next to a new
// clusterRepository and pin IsChartStatusInfoOutdated to true forever.
func TestApplicationReleaseReplacesLastAppliedChartWholesale(t *testing.T) {
	app := namespacedApp("team-a", "my-app", "stable")
	rel := NewApplicationRelease(app)

	if rel.LastAppliedChart() != nil {
		t.Fatal("LastAppliedChart must be nil before a first deployment")
	}

	rel.SetLastAppliedChart(rel.ChartRef())
	if app.Status.LastAppliedChart.Repository != "stable" || app.Status.LastAppliedChart.ClusterRepository != "" {
		t.Fatalf("after a namespaced apply: %+v", app.Status.LastAppliedChart)
	}
	if rel.IsChartStatusInfoOutdated() {
		t.Fatal("the applied chart is the desired one")
	}

	app.Spec.Chart.Repository = ""
	app.Spec.Chart.ClusterRepository = "shared"
	if !rel.IsChartStatusInfoOutdated() {
		t.Fatal("moving to a cluster repository is a chart change")
	}

	rel.SetLastAppliedChart(rel.ChartRef())
	if app.Status.LastAppliedChart.ClusterRepository != "shared" || app.Status.LastAppliedChart.Repository != "" {
		t.Fatalf("the record must be replaced, not merged: %+v", app.Status.LastAppliedChart)
	}
	if got := rel.LastAppliedChart(); got == nil || *got != rel.ChartRef() {
		t.Fatalf("LastAppliedChart = %+v, want the applied ref", got)
	}
}

func TestApplicationRepositoryResolverPicksTheKind(t *testing.T) {
	namespaced := &helmv1alpha1.HelmApplicationRepository{
		ObjectMeta: metav1.ObjectMeta{Name: "stable", Namespace: "team-a"},
		Spec:       helmv1alpha1.RepositorySpec{URL: "https://charts.example.invalid/stable"},
	}
	cluster := &helmv1alpha1.HelmClusterApplicationRepository{
		ObjectMeta: metav1.ObjectMeta{Name: "shared"},
		Spec:       helmv1alpha1.RepositorySpec{URL: "oci://ghcr.io/example/charts"},
	}
	resolver := NewApplicationRepositoryResolver(applicationClient(t, namespaced, cluster))

	repo, _, err := resolver.Resolve(context.Background(), source.RepositoryRef{Kind: helmv1alpha1.HelmApplicationRepositoryKind, Namespace: "team-a", Name: "stable"})
	if err != nil {
		t.Fatalf("Resolve(namespaced) returned %v", err)
	}
	if repo.OwnerGVK() != helmv1alpha1.HelmApplicationRepositoryGVK || repo.Namespace() != "team-a" {
		t.Fatalf("resolved %v %q", repo.OwnerGVK(), repo.Namespace())
	}

	repo, _, err = resolver.Resolve(context.Background(), source.RepositoryRef{Kind: helmv1alpha1.HelmClusterApplicationRepositoryKind, Name: "shared"})
	if err != nil {
		t.Fatalf("Resolve(cluster) returned %v", err)
	}
	if repo.OwnerGVK() != helmv1alpha1.HelmClusterApplicationRepositoryGVK {
		t.Fatalf("resolved %v", repo.OwnerGVK())
	}

	if _, _, err := resolver.Resolve(context.Background(), source.RepositoryRef{Kind: "Something", Name: "x"}); err == nil {
		t.Fatal("an unknown repository kind must be an error")
	}
}

// TestListApplicationReleasesIsScopedByRepositoryNamespace is the property the
// namespaced catalog depends on: a repository named "stable" in team-a must not see
// the applications using "stable" in team-b.
func TestListApplicationReleasesIsScopedByRepositoryNamespace(t *testing.T) {
	c := applicationClient(t,
		namespacedApp("team-a", "a1", "stable"),
		namespacedApp("team-a", "a2", "stable"),
		namespacedApp("team-b", "b1", "stable"),
		clusterApp("team-b", "b2", "stable"),
	)
	list := ListApplicationReleases(c)

	teamA := NewApplicationRepository(&helmv1alpha1.HelmApplicationRepository{ObjectMeta: metav1.ObjectMeta{Name: "stable", Namespace: "team-a"}})
	got, err := list(context.Background(), teamA, "")
	if err != nil {
		t.Fatalf("listing: %v", err)
	}
	if names := releaseNames(got); !reflect.DeepEqual(names, map[string]bool{"a1": true, "a2": true}) {
		t.Fatalf("consumers of team-a/stable = %v", names)
	}

	shared := NewClusterApplicationRepository(&helmv1alpha1.HelmClusterApplicationRepository{ObjectMeta: metav1.ObjectMeta{Name: "stable"}})
	got, err = list(context.Background(), shared, "podinfo")
	if err != nil {
		t.Fatalf("listing: %v", err)
	}
	if names := releaseNames(got); !reflect.DeepEqual(names, map[string]bool{"b2": true}) {
		t.Fatalf("consumers of the cluster repository stable/podinfo = %v", names)
	}
}
