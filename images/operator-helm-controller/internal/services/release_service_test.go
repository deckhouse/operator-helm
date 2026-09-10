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

	helmv2 "github.com/werf/3p-helm-controller/api/v2"
	sourcev1 "github.com/werf/nelm-source-controller/api/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/deckhouse/operator-helm/internal/adapter"
	"github.com/deckhouse/operator-helm/internal/source"
	"github.com/deckhouse/operator-helm/internal/utils"
)

func newReleaseService(t *testing.T, objects ...client.Object) (*ReleaseService, client.Client) {
	t.Helper()

	scheme := testScheme(t)
	for _, add := range []func(*runtime.Scheme) error{
		sourcev1.AddToScheme,
		helmv2.AddToScheme,
	} {
		if err := add(scheme); err != nil {
			t.Fatalf("registering scheme: %v", err)
		}
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()

	return NewReleaseService(c, scheme, testNamespace), c
}

func ensureRelease(t *testing.T, service *ReleaseService, c client.Client, rel source.Release) *helmv2.HelmRelease {
	t.Helper()

	if res := service.EnsureHelmRelease(context.Background(), rel, utils.InternalHelmRepository, ""); res.Status.Err != nil {
		t.Fatalf("EnsureHelmRelease returned %v", res.Status.Err)
	}

	release := &helmv2.HelmRelease{}
	key := client.ObjectKey{Namespace: testNamespace, Name: rel.InternalNames().HelmRelease}
	if err := c.Get(context.Background(), key, release); err != nil {
		t.Fatalf("helm release was not created: %v", err)
	}

	return release
}

// TestEnsureHelmReleaseAppliesAnApplicationAsItsOwnIdentity pins the two fields
// that make an application release run as itself: helm-controller impersonates the
// named account, and because that account only has rights inside the application's
// namespace, the release storage has to live there too.
func TestEnsureHelmReleaseAppliesAnApplicationAsItsOwnIdentity(t *testing.T) {
	rel := adapter.NewApplicationRelease(testApplication())
	service, c := newReleaseService(t)

	release := ensureRelease(t, service, c, rel)

	if want := rel.InternalNames().ServiceAccount; release.Spec.ServiceAccountName != want {
		t.Fatalf("serviceAccountName = %q, want %q", release.Spec.ServiceAccountName, want)
	}
	if release.Spec.StorageNamespace != "team-a" {
		t.Fatalf("storageNamespace = %q, want the application namespace", release.Spec.StorageNamespace)
	}
}

// TestEnsureHelmReleaseLeavesTheAddonIdentityAlone is the complement: an addon
// family names no service account, so helm-controller keeps its own identity and
// the default storage location. Setting either field for it would change where
// every existing addon's release history is kept.
func TestEnsureHelmReleaseLeavesTheAddonIdentityAlone(t *testing.T) {
	rel := adapter.NewAddonRelease(testAddon())
	service, c := newReleaseService(t)

	release := ensureRelease(t, service, c, rel)

	if release.Spec.ServiceAccountName != "" {
		t.Fatalf("serviceAccountName = %q, want it unset", release.Spec.ServiceAccountName)
	}
	if release.Spec.StorageNamespace != "" {
		t.Fatalf("storageNamespace = %q, want it unset", release.Spec.StorageNamespace)
	}
}

// TestEnsureHelmReleaseKeepsForeignLabels is the same guarantee on the release:
// our labels are merged in, not written over what someone else put there.
func TestEnsureHelmReleaseKeepsForeignLabels(t *testing.T) {
	rel := adapter.NewApplicationRelease(testApplication())
	existing := &helmv2.HelmRelease{
		ObjectMeta: metav1.ObjectMeta{
			Name:      rel.InternalNames().HelmRelease,
			Namespace: testNamespace,
			Labels:    map[string]string{"cost-center": "team-a"},
		},
	}
	service, c := newReleaseService(t, existing)

	release := ensureRelease(t, service, c, rel)

	if release.Labels["cost-center"] != "team-a" {
		t.Fatalf("labels = %v, want the foreign label preserved", release.Labels)
	}
	for key, want := range rel.SourceLabels() {
		if release.Labels[key] != want {
			t.Fatalf("label %q = %q, want %q", key, release.Labels[key], want)
		}
	}
}
