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
	"reflect"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
	"github.com/deckhouse/operator-helm/internal/source"
	"github.com/deckhouse/operator-helm/internal/utils"
)

func addonRepo(name string) *helmv1alpha1.HelmClusterAddonRepository {
	return &helmv1alpha1.HelmClusterAddonRepository{
		ObjectMeta: metav1.ObjectMeta{Name: name, Generation: 3, Annotations: map[string]string{helmv1alpha1.AnnotationForceReconcile: ""}},
		Spec: helmv1alpha1.RepositorySpec{
			URL:                "oci://ghcr.io/example/charts",
			Auth:               &helmv1alpha1.RepositoryAuth{Username: "u", Password: "p"},
			CACertificate:      "-----BEGIN CERTIFICATE-----",
			InsecureSkipVerify: true,
		},
	}
}

// TestAddonRepositoryKeepsTheReleasedNamesAndLabels pins the adapter to what the
// addon controller writes today: the same three internal names and the same two
// labels. Anything else here would re-create live objects.
func TestAddonRepositoryKeepsTheReleasedNamesAndLabels(t *testing.T) {
	obj := addonRepo("example")
	repo := NewAddonRepository(obj)

	if repo.Object() != obj {
		t.Fatal("Object must return the wrapped object itself, it is what the client reads and patches")
	}
	if repo.Name() != "example" || repo.Namespace() != "" || repo.Generation() != 3 {
		t.Fatalf("identity = %q/%q gen %d, want example/\"\" gen 3", repo.Namespace(), repo.Name(), repo.Generation())
	}
	if repo.OwnerGVK() != helmv1alpha1.HelmClusterAddonRepositoryGVK {
		t.Fatalf("OwnerGVK = %v", repo.OwnerGVK())
	}
	if repo.URL() != obj.Spec.URL || repo.Auth() != obj.Spec.Auth ||
		repo.CACertificate() != obj.Spec.CACertificate || repo.InsecureSkipVerify() != obj.Spec.InsecureSkipVerify {
		t.Fatal("spec accessors must proxy the spec fields")
	}
	if repo.Status() != &obj.Status {
		t.Fatal("Status must point at the wrapped object's status so a write through it lands on the object")
	}
	if !repo.ForceReconcileRequired() {
		t.Fatal("ForceReconcileRequired must follow the annotation")
	}

	wantLabels := map[string]string{
		helmv1alpha1.LabelManagedBy:                            helmv1alpha1.LabelManagedByValue,
		helmv1alpha1.HelmClusterAddonRepositoryLabelSourceName: "example",
	}
	if got := repo.SourceLabels(); !reflect.DeepEqual(got, wantLabels) {
		t.Fatalf("SourceLabels = %v, want %v", got, wantLabels)
	}

	wantNames := source.InternalNames{
		HelmRepository: utils.GetInternalHelmRepositoryName("example"),
		AuthSecret:     utils.GetInternalRepositoryAuthSecretName("example"),
		TLSSecret:      utils.GetInternalRepositoryTLSSecretName("example"),
	}
	if got := repo.InternalNames(); got != wantNames {
		t.Fatalf("InternalNames = %+v, want %+v", got, wantNames)
	}
}

func TestApplicationRepositoryCarriesNamespaceInLabelsAndNames(t *testing.T) {
	obj := &helmv1alpha1.HelmApplicationRepository{
		ObjectMeta: metav1.ObjectMeta{Name: "stable", Namespace: "team-a", Generation: 1},
		Spec:       helmv1alpha1.RepositorySpec{URL: "https://charts.example.invalid/stable"},
	}
	repo := NewApplicationRepository(obj)

	if repo.Namespace() != "team-a" || repo.Name() != "stable" {
		t.Fatalf("identity = %q/%q", repo.Namespace(), repo.Name())
	}
	if repo.OwnerGVK() != helmv1alpha1.HelmApplicationRepositoryGVK {
		t.Fatalf("OwnerGVK = %v", repo.OwnerGVK())
	}

	wantLabels := map[string]string{
		helmv1alpha1.LabelManagedBy:                           helmv1alpha1.LabelManagedByValue,
		helmv1alpha1.HelmApplicationRepositoryLabelSourceName: "stable",
		helmv1alpha1.LabelSourceNamespace:                     "team-a",
	}
	if got := repo.SourceLabels(); !reflect.DeepEqual(got, wantLabels) {
		t.Fatalf("SourceLabels = %v, want %v", got, wantLabels)
	}

	want := source.InternalNames{
		HelmRepository: "hapr-team-a-stable-42df68033b1e",
		AuthSecret:     utils.DerivedName("hapr-auth", helmv1alpha1.HelmApplicationRepositoryKind, "team-a", "stable"),
		TLSSecret:      utils.DerivedName("hapr-tls", helmv1alpha1.HelmApplicationRepositoryKind, "team-a", "stable"),
	}
	if got := repo.InternalNames(); got != want {
		t.Fatalf("InternalNames = %+v, want %+v", got, want)
	}
}

// TestSameNamedRepositoriesNeverShareInternalNames is the property the whole
// naming scheme exists for: every internal object of every repository lives in one
// namespace, so two sources that share a name must still derive distinct names —
// across namespaces and across kinds alike.
func TestSameNamedRepositoriesNeverShareInternalNames(t *testing.T) {
	spec := helmv1alpha1.RepositorySpec{URL: "https://charts.example.invalid/stable"}

	teamA := NewApplicationRepository(&helmv1alpha1.HelmApplicationRepository{
		ObjectMeta: metav1.ObjectMeta{Name: "stable", Namespace: "team-a"}, Spec: spec,
	})
	teamB := NewApplicationRepository(&helmv1alpha1.HelmApplicationRepository{
		ObjectMeta: metav1.ObjectMeta{Name: "stable", Namespace: "team-b"}, Spec: spec,
	})
	cluster := NewClusterApplicationRepository(&helmv1alpha1.HelmClusterApplicationRepository{
		ObjectMeta: metav1.ObjectMeta{Name: "stable"}, Spec: spec,
	})
	addon := NewAddonRepository(&helmv1alpha1.HelmClusterAddonRepository{
		ObjectMeta: metav1.ObjectMeta{Name: "stable"}, Spec: spec,
	})

	seen := map[string]string{}
	for label, repo := range map[string]source.Repository{"team-a": teamA, "team-b": teamB, "cluster": cluster, "addon": addon} {
		names := repo.InternalNames()
		for _, n := range []string{names.HelmRepository, names.AuthSecret, names.TLSSecret} {
			if owner, dup := seen[n]; dup {
				t.Fatalf("%s and %s derive the same internal name %q", owner, label, n)
			}
			seen[n] = label
			if len(n) > 63 {
				t.Fatalf("%q is %d characters, the limit is 63", n, len(n))
			}
		}
	}
}

func TestClusterApplicationRepositoryHasNoNamespaceLabel(t *testing.T) {
	repo := NewClusterApplicationRepository(&helmv1alpha1.HelmClusterApplicationRepository{
		ObjectMeta: metav1.ObjectMeta{Name: "shared"},
		Spec:       helmv1alpha1.RepositorySpec{URL: "oci://ghcr.io/example/charts"},
	})

	if repo.Namespace() != "" {
		t.Fatalf("Namespace = %q, want empty", repo.Namespace())
	}
	if repo.OwnerGVK() != helmv1alpha1.HelmClusterApplicationRepositoryGVK {
		t.Fatalf("OwnerGVK = %v", repo.OwnerGVK())
	}

	wantLabels := map[string]string{
		helmv1alpha1.LabelManagedBy:                                  helmv1alpha1.LabelManagedByValue,
		helmv1alpha1.HelmClusterApplicationRepositoryLabelSourceName: "shared",
	}
	if got := repo.SourceLabels(); !reflect.DeepEqual(got, wantLabels) {
		t.Fatalf("SourceLabels = %v, want %v", got, wantLabels)
	}
	if got := repo.InternalNames().HelmRepository; got != "hcapr-shared-"+utils.GetHash(helmv1alpha1.HelmClusterApplicationRepositoryKind+"//shared") {
		t.Fatalf("HelmRepository name = %q", got)
	}
}

func TestEmptyConstructorsReturnAddressableObjectsOfTheirKind(t *testing.T) {
	cases := map[string]struct {
		repo source.Repository
		gvk  string
	}{
		"addon":               {EmptyAddonRepository(), helmv1alpha1.HelmClusterAddonRepositoryKind},
		"application":         {EmptyApplicationRepository(), helmv1alpha1.HelmApplicationRepositoryKind},
		"cluster application": {EmptyClusterApplicationRepository(), helmv1alpha1.HelmClusterApplicationRepositoryKind},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if tc.repo.Object() == nil {
				t.Fatal("Object must not be nil: the reconciler reads the API object into it")
			}
			if tc.repo.OwnerGVK().Kind != tc.gvk {
				t.Fatalf("kind = %q, want %q", tc.repo.OwnerGVK().Kind, tc.gvk)
			}
		})
	}
}
