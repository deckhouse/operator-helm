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

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/deckhouse/operator-helm/api/naming"
	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
	"github.com/deckhouse/operator-helm/internal/source"
	"github.com/deckhouse/operator-helm/internal/utils"
)

func addonRelease(name string) *helmv1alpha1.HelmClusterAddon {
	return &helmv1alpha1.HelmClusterAddon{
		ObjectMeta: metav1.ObjectMeta{Name: name, Generation: 2, Annotations: map[string]string{helmv1alpha1.AnnotationForceReconcile: ""}},
		Spec: helmv1alpha1.HelmClusterAddonSpec{
			Namespace:   "app",
			Maintenance: string(helmv1alpha1.NoResourceReconciliation),
			Values:      &apiextensionsv1.JSON{Raw: []byte(`{"replicas":2}`)},
			Chart: helmv1alpha1.HelmClusterAddonChartRef{
				HelmClusterAddonRepository: "example",
				HelmClusterAddonChartName:  "podinfo",
				Version:                    "6.7.1",
			},
		},
	}
}

// TestAddonReleaseKeepsTheReleasedNamesAndLabels pins the adapter to what the addon
// controller writes today. The three internal names come from the frozen naming
// functions; the release name is the addon name itself for every name Helm accepts.
func TestAddonReleaseKeepsTheReleasedNamesAndLabels(t *testing.T) {
	obj := addonRelease("consumer")
	rel := NewAddonRelease(obj)

	if rel.Object() != obj {
		t.Fatal("Object must return the wrapped object itself")
	}
	if rel.Kind() != helmv1alpha1.HelmClusterAddonKind || rel.Name() != "consumer" || rel.Namespace() != "" || rel.Generation() != 2 {
		t.Fatalf("identity = %s %q/%q gen %d", rel.Kind(), rel.Namespace(), rel.Name(), rel.Generation())
	}

	wantRef := source.ChartRef{
		Repository: source.RepositoryRef{Kind: helmv1alpha1.HelmClusterAddonRepositoryKind, Name: "example"},
		Chart:      "podinfo",
		Version:    "6.7.1",
	}
	if got := rel.ChartRef(); got != wantRef {
		t.Fatalf("ChartRef = %+v, want %+v", got, wantRef)
	}
	if rel.TargetNamespace() != "app" {
		t.Fatalf("TargetNamespace = %q, want the spec namespace", rel.TargetNamespace())
	}
	if rel.Values() != obj.Spec.Values {
		t.Fatal("Values must point at the spec values")
	}
	if !rel.MaintenanceActivated() || rel.MaintenanceEnabled() {
		t.Fatal("maintenance is requested by the spec but not yet reflected by the Managed condition")
	}
	if !rel.ForceReconcileRequired() {
		t.Fatal("ForceReconcileRequired must follow the annotation")
	}
	if rel.ReleaseName() != "consumer" {
		t.Fatalf("ReleaseName = %q, want the addon name itself", rel.ReleaseName())
	}

	wantLabels := map[string]string{
		helmv1alpha1.LabelManagedBy:                  helmv1alpha1.LabelManagedByValue,
		helmv1alpha1.HelmClusterAddonLabelSourceName: "consumer",
	}
	if got := rel.SourceLabels(); !reflect.DeepEqual(got, wantLabels) {
		t.Fatalf("SourceLabels = %v, want %v", got, wantLabels)
	}
	wantChartLabels := map[string]string{
		helmv1alpha1.LabelManagedBy:                       helmv1alpha1.LabelManagedByValue,
		helmv1alpha1.HelmClusterAddonLabelSourceName:      "consumer",
		helmv1alpha1.HelmClusterAddonChartLabelSourceName: naming.HelmClusterAddonChartName("example", "podinfo"),
	}
	if got := rel.HelmChartLabels(); !reflect.DeepEqual(got, wantChartLabels) {
		t.Fatalf("HelmChartLabels = %v, want %v", got, wantChartLabels)
	}

	wantNames := source.ReleaseNames{
		HelmChart:     utils.GetInternalHelmChartName("consumer"),
		HelmRelease:   utils.GetInternalHelmReleaseName("consumer"),
		OCIRepository: utils.GetInternalOCIRepositoryName("consumer"),
	}
	if got := rel.InternalNames(); got != wantNames {
		t.Fatalf("InternalNames = %+v, want %+v (no service account for the addon family)", got, wantNames)
	}
}

func TestAddonReleaseStatusAccessorsWriteThroughToTheObject(t *testing.T) {
	obj := addonRelease("consumer")
	rel := NewAddonRelease(obj)

	if rel.LastAppliedChart() != nil {
		t.Fatal("LastAppliedChart must be nil before a first deployment")
	}
	if !rel.IsChartStatusInfoOutdated() {
		t.Fatal("a release that was never applied is outdated")
	}

	rel.SetLastAppliedChart(rel.ChartRef())

	want := &helmv1alpha1.HelmClusterAddonLastAppliedChartRef{
		HelmClusterAddonRepository: "example", HelmClusterAddonChartName: "podinfo", Version: "6.7.1",
	}
	if !reflect.DeepEqual(obj.Status.LastAppliedChart, want) {
		t.Fatalf("status.lastAppliedChart = %+v, want %+v", obj.Status.LastAppliedChart, want)
	}
	if got := rel.LastAppliedChart(); got == nil || *got != rel.ChartRef() {
		t.Fatalf("LastAppliedChart = %+v, want the applied ref", got)
	}
	if rel.IsChartStatusInfoOutdated() {
		t.Fatal("after applying the desired chart the status is current")
	}

	rel.SetLastAppliedValues(obj.Spec.Values)
	if obj.Status.LastAppliedValues != obj.Spec.Values || rel.LastAppliedValues() != obj.Spec.Values {
		t.Fatal("LastAppliedValues must write through to the object")
	}

	now := metav1.Now()
	rel.SetLastForceReconcileTime(now)
	if obj.Status.LastForceReconcileTime == nil || !obj.Status.LastForceReconcileTime.Equal(&now) {
		t.Fatalf("status.lastForceReconcileTime = %v, want %v", obj.Status.LastForceReconcileTime, now)
	}
}

func TestAddonReleaseBoundsALongReleaseName(t *testing.T) {
	long := "abcdefghijklmnopqrstuvwxyz-abcdefghijklmnopqrstuvwxyz-abcdefg"
	rel := NewAddonRelease(addonRelease(long))

	if got := rel.ReleaseName(); got != utils.HelmReleaseName(long) || len(got) > 53 {
		t.Fatalf("ReleaseName = %q (%d chars), want the bounded name", got, len(got))
	}
}

func TestAddonRepositoryResolverLoadsTheRepositoryAndTheAddonCatalog(t *testing.T) {
	repo := &helmv1alpha1.HelmClusterAddonRepository{
		ObjectMeta: metav1.ObjectMeta{Name: "example"},
		Spec:       helmv1alpha1.RepositorySpec{URL: "oci://ghcr.io/example/charts"},
	}
	c := addonClient(t, repo)

	got, cat, err := NewAddonRepositoryResolver(c).Resolve(context.Background(), source.RepositoryRef{
		Kind: helmv1alpha1.HelmClusterAddonRepositoryKind, Name: "example",
	})
	if err != nil {
		t.Fatalf("Resolve returned %v", err)
	}
	if got.Name() != "example" || got.URL() != repo.Spec.URL || got.OwnerGVK() != helmv1alpha1.HelmClusterAddonRepositoryGVK {
		t.Fatalf("resolved repository = %q %q %v", got.Name(), got.URL(), got.OwnerGVK())
	}
	if cat == nil {
		t.Fatal("Resolve must return the addon catalog")
	}

	if _, _, err := NewAddonRepositoryResolver(c).Resolve(context.Background(), source.RepositoryRef{
		Kind: helmv1alpha1.HelmClusterAddonRepositoryKind, Name: "missing",
	}); err == nil {
		t.Fatal("Resolve of a missing repository must fail")
	}
}

// TestListAddonReleasesUsesTheIndexes pins the two lookups a repository needs: every
// consumer of the repository, and only the consumers of one chart.
func TestListAddonReleasesUsesTheIndexes(t *testing.T) {
	podinfo := addonRelease("podinfo-consumer")
	nginx := addonRelease("nginx-consumer")
	nginx.Spec.Chart.HelmClusterAddonChartName = "nginx"
	foreign := addonRelease("foreign")
	foreign.Spec.Chart.HelmClusterAddonRepository = "another"

	c := addonClient(t, podinfo, nginx, foreign)
	repo := NewAddonRepository(&helmv1alpha1.HelmClusterAddonRepository{ObjectMeta: metav1.ObjectMeta{Name: "example"}})
	list := ListAddonReleases(c)

	all, err := list(context.Background(), repo, "")
	if err != nil {
		t.Fatalf("listing consumers: %v", err)
	}
	if names := releaseNames(all); !reflect.DeepEqual(names, map[string]bool{"podinfo-consumer": true, "nginx-consumer": true}) {
		t.Fatalf("consumers of the repository = %v", names)
	}

	ofChart, err := list(context.Background(), repo, "nginx")
	if err != nil {
		t.Fatalf("listing consumers of a chart: %v", err)
	}
	if names := releaseNames(ofChart); !reflect.DeepEqual(names, map[string]bool{"nginx-consumer": true}) {
		t.Fatalf("consumers of nginx = %v", names)
	}
}

func releaseNames(releases []source.Release) map[string]bool {
	out := make(map[string]bool, len(releases))
	for _, rel := range releases {
		out[rel.Name()] = true
	}

	return out
}
