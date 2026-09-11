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

package resolver

import (
	"context"
	"reflect"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
)

func TestFamilyForRejectsAnUnknownKind(t *testing.T) {
	if _, ok := familyFor("somethingelse"); ok {
		t.Fatal("an unknown kind must not resolve to a family")
	}
}

// TestAddonFamilyReadsTheClusterScopedRepositoryAndCatalog pins that the addon
// family is unchanged: a cluster-scoped repository read by name alone, its catalog
// object named by the shared scheme, and internal objects found by the one label
// the operator writes for it.
func TestAddonFamilyReadsTheClusterScopedRepositoryAndCatalog(t *testing.T) {
	repo := &helmv1alpha1.HelmClusterAddonRepository{
		ObjectMeta: metav1.ObjectMeta{Name: "example"},
		Spec: helmv1alpha1.RepositorySpec{
			URL:                "oci://ghcr.io/example/charts",
			Auth:               &helmv1alpha1.RepositoryAuth{Username: "u", Password: "p"},
			CACertificate:      "-----BEGIN CERTIFICATE-----",
			InsecureSkipVerify: true,
		},
	}
	// The name is a literal, not built through the same apinaming helper the family
	// under test calls: otherwise a wrong naming scheme in the family could never be
	// caught, since the fixture would always agree with whatever the family did.
	chart := &helmv1alpha1.HelmClusterAddonChart{
		ObjectMeta: metav1.ObjectMeta{Name: "example-chart-podinfo-aa661c3516b2"},
		Status:     helmv1alpha1.ChartCatalogStatus{Versions: []helmv1alpha1.ChartVersion{{Version: "6.7.1"}}},
	}

	family, ok := familyFor(RepositoryKindHelmClusterAddon)
	if !ok {
		t.Fatal("the addon kind must resolve to a family")
	}
	if family.Namespaced {
		t.Fatal("HelmClusterAddonRepository is cluster-scoped")
	}

	c := newTestResolver(t, repo, chart).client

	spec, err := family.GetRepository(context.Background(), c, "", "example")
	if err != nil {
		t.Fatalf("GetRepository returned %v", err)
	}
	want := &repositorySpec{
		URL:                repo.Spec.URL,
		Auth:               repo.Spec.Auth,
		CACertificate:      repo.Spec.CACertificate,
		InsecureSkipVerify: true,
	}
	if !reflect.DeepEqual(spec, want) {
		t.Fatalf("repository spec = %+v, want %+v", spec, want)
	}

	versions, err := family.ChartVersions(context.Background(), c, "", "example", "podinfo")
	if err != nil {
		t.Fatalf("ChartVersions returned %v", err)
	}
	if len(versions) != 1 || versions[0].Version != "6.7.1" {
		t.Fatalf("versions = %+v, want [6.7.1]", versions)
	}

	wantLabels := map[string]string{helmv1alpha1.HelmClusterAddonRepositoryLabelSourceName: "example"}
	if got := family.InternalLabels("", "example"); !reflect.DeepEqual(got, wantLabels) {
		t.Fatalf("InternalLabels = %v, want %v", got, wantLabels)
	}
}

func TestAddonFamilyReportsAMissingRepositoryAsNotFound(t *testing.T) {
	family, _ := familyFor(RepositoryKindHelmClusterAddon)
	c := newTestResolver(t).client

	_, err := family.GetRepository(context.Background(), c, "", "missing")
	if !apierrors.IsNotFound(err) {
		t.Fatalf("err = %v, want a NotFound so the caller can report repository_not_found", err)
	}
}

func TestAddonFamilyReportsAMissingCatalogAsNotFound(t *testing.T) {
	family, _ := familyFor(RepositoryKindHelmClusterAddon)
	c := newTestResolver(t).client

	_, err := family.ChartVersions(context.Background(), c, "", "example", "podinfo")
	if !apierrors.IsNotFound(err) {
		t.Fatalf("err = %v, want a NotFound so the caller can report pending", err)
	}
}

// TestApplicationFamiliesReadTheirOwnObjects pins the two application kinds: the
// namespaced one reads its repository and catalog from the request namespace, the
// cluster one from the cluster scope, and each recognises its own internal objects.
func TestApplicationFamiliesReadTheirOwnObjects(t *testing.T) {
	namespaced := &helmv1alpha1.HelmApplicationRepository{
		ObjectMeta: metav1.ObjectMeta{Name: "stable", Namespace: "team-a"},
		Spec:       helmv1alpha1.RepositorySpec{URL: "https://charts.example.invalid/stable"},
	}
	// Both catalog objects are named with a literal, not apinaming.*ChartName, for the
	// same reason as the addon fixture above.
	namespacedChart := &helmv1alpha1.HelmApplicationChart{
		ObjectMeta: metav1.ObjectMeta{Name: "stable-chart-podinfo-cb815671ddc8", Namespace: "team-a"},
		Status:     helmv1alpha1.ChartCatalogStatus{Versions: []helmv1alpha1.ChartVersion{{Version: "6.7.1"}}},
	}
	cluster := &helmv1alpha1.HelmClusterApplicationRepository{
		ObjectMeta: metav1.ObjectMeta{Name: "shared"},
		Spec:       helmv1alpha1.RepositorySpec{URL: "oci://ghcr.io/example/charts"},
	}
	clusterChart := &helmv1alpha1.HelmClusterApplicationChart{
		ObjectMeta: metav1.ObjectMeta{Name: "shared-chart-podinfo-83fd5aa25c3a"},
		Status:     helmv1alpha1.ChartCatalogStatus{Versions: []helmv1alpha1.ChartVersion{{Version: "1.2.3"}}},
	}

	c := newTestResolver(t, namespaced, namespacedChart, cluster, clusterChart).client

	nsFamily, ok := familyFor(RepositoryKindHelmApplication)
	if !ok || !nsFamily.Namespaced {
		t.Fatalf("the namespaced application kind must resolve to a namespaced family (ok=%v)", ok)
	}

	spec, err := nsFamily.GetRepository(context.Background(), c, "team-a", "stable")
	if err != nil || spec.URL != namespaced.Spec.URL {
		t.Fatalf("namespaced repository = %+v, err = %v", spec, err)
	}
	versions, err := nsFamily.ChartVersions(context.Background(), c, "team-a", "stable", "podinfo")
	if err != nil || len(versions) != 1 || versions[0].Version != "6.7.1" {
		t.Fatalf("namespaced versions = %+v, err = %v", versions, err)
	}
	wantNSLabels := map[string]string{
		helmv1alpha1.HelmApplicationRepositoryLabelSourceName: "stable",
		helmv1alpha1.LabelSourceNamespace:                     "team-a",
	}
	if got := nsFamily.InternalLabels("team-a", "stable"); !reflect.DeepEqual(got, wantNSLabels) {
		t.Fatalf("namespaced internal labels = %v, want %v", got, wantNSLabels)
	}

	// A repository of the same name in another namespace is a different repository.
	if _, err := nsFamily.GetRepository(context.Background(), c, "team-b", "stable"); !apierrors.IsNotFound(err) {
		t.Fatalf("err = %v, want NotFound for a same-named repository in another namespace", err)
	}

	clusterFamily, ok := familyFor(RepositoryKindHelmClusterApplication)
	if !ok || clusterFamily.Namespaced {
		t.Fatalf("the cluster application kind must resolve to a cluster-scoped family (ok=%v)", ok)
	}

	spec, err = clusterFamily.GetRepository(context.Background(), c, "", "shared")
	if err != nil || spec.URL != cluster.Spec.URL {
		t.Fatalf("cluster repository = %+v, err = %v", spec, err)
	}
	versions, err = clusterFamily.ChartVersions(context.Background(), c, "", "shared", "podinfo")
	if err != nil || len(versions) != 1 || versions[0].Version != "1.2.3" {
		t.Fatalf("cluster versions = %+v, err = %v", versions, err)
	}
	wantClusterLabels := map[string]string{helmv1alpha1.HelmClusterApplicationRepositoryLabelSourceName: "shared"}
	if got := clusterFamily.InternalLabels("", "shared"); !reflect.DeepEqual(got, wantClusterLabels) {
		t.Fatalf("cluster internal labels = %v, want %v", got, wantClusterLabels)
	}
}

func TestRequireNamespaceRejectsTheWrongRequestShape(t *testing.T) {
	nsFamily, _ := familyFor(RepositoryKindHelmApplication)
	if err := nsFamily.requireNamespace(""); err == nil {
		t.Fatal("a namespaced kind without a namespace must be rejected")
	}
	if err := nsFamily.requireNamespace("team-a"); err != nil {
		t.Fatalf("a namespaced kind with a namespace must be accepted, got %v", err)
	}

	addon, _ := familyFor(RepositoryKindHelmClusterAddon)
	if err := addon.requireNamespace("team-a"); err != nil {
		t.Fatalf("a cluster-scoped kind with a namespace must be accepted: the namespace is not part of its identity, got %v", err)
	}
	if err := addon.requireNamespace(""); err != nil {
		t.Fatalf("a cluster-scoped kind without a namespace must be accepted, got %v", err)
	}
}
