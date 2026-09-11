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

	"github.com/werf/3p-fluxcd-pkg/apis/meta"
	sourcev1 "github.com/werf/nelm-source-controller/api/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/deckhouse/operator-helm/internal/adapter"
	"github.com/deckhouse/operator-helm/internal/utils"
)

// TestForceReconcileConsumersStampsOnlyItsOwnAddons covers the force
// reconcile annotation applied to an oci:// HelmClusterAddonRepository: unlike the
// helm:// path, where the internal HelmRepository re-indexes and the HelmCharts
// follow, an OCI repository has no intermediate source object, so the request must
// be pushed onto the internal OCIRepository of each addon that references it - and
// only of those addons.
func TestForceReconcileConsumersStampsOnlyItsOwnAddons(t *testing.T) {
	addon := testAddon()
	foreign := testAddon()
	foreign.Name = "foreign"
	foreign.Spec.Chart.HelmClusterAddonRepository = "another"

	_, c := newOCIRepoService(t,
		addon, foreign,
		internalOCIRepository(addon.Name), internalOCIRepository(foreign.Name),
	)
	service := NewForceService(c, testNamespace, adapter.ListAddonReleases(c))

	if err := service.ForceReconcileConsumers(context.Background(), adapter.NewAddonRepository(ociTestRepository())); err != nil {
		t.Fatalf("forcing internal repositories: %v", err)
	}

	ociRepo := &sourcev1.OCIRepository{}
	key := client.ObjectKey{Name: utils.GetInternalOCIRepositoryName(addon.Name), Namespace: testNamespace}
	if err := c.Get(context.Background(), key, ociRepo); err != nil {
		t.Fatalf("getting oci repository: %v", err)
	}
	if ociRepo.Annotations[meta.ReconcileRequestAnnotation] == "" {
		t.Errorf("%s must be stamped on the oci repository of the addon", meta.ReconcileRequestAnnotation)
	}
	if ociRepo.Annotations[meta.ForceRequestAnnotation] == "" {
		t.Errorf("%s must be stamped on the oci repository of the addon", meta.ForceRequestAnnotation)
	}

	foreignRepo := &sourcev1.OCIRepository{}
	key = client.ObjectKey{Name: utils.GetInternalOCIRepositoryName(foreign.Name), Namespace: testNamespace}
	if err := c.Get(context.Background(), key, foreignRepo); err != nil {
		t.Fatalf("getting foreign oci repository: %v", err)
	}
	if _, found := foreignRepo.Annotations[meta.ReconcileRequestAnnotation]; found {
		t.Errorf("%s must not be stamped on an addon of another repository", meta.ReconcileRequestAnnotation)
	}
}

// TestForceReconcileConsumersToleratesMissingSource covers the addon
// that has no internal OCIRepository yet - it has just been created, or it never
// reached the point of building one. A force on the repository must not fail
// because of it, otherwise the request is retried forever.
func TestForceReconcileConsumersToleratesMissingSource(t *testing.T) {
	addon := testAddon()
	_, c := newOCIRepoService(t, addon)
	service := NewForceService(c, testNamespace, adapter.ListAddonReleases(c))

	if err := service.ForceReconcileConsumers(context.Background(), adapter.NewAddonRepository(ociTestRepository())); err != nil {
		t.Fatalf("a missing internal oci repository must not fail the force request: %v", err)
	}
}
