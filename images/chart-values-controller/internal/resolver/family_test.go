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
	chart := chartWithVersions("example", "podinfo", helmv1alpha1.ChartVersion{Version: "6.7.1"})

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
