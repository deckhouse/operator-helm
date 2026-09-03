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
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
	"github.com/deckhouse/operator-helm/internal/utils"
)

func newChartService(t *testing.T, objects ...client.Object) (*ChartService, client.Client) {
	t.Helper()

	scheme := testScheme(t)
	if err := sourcev1.AddToScheme(scheme); err != nil {
		t.Fatalf("registering source scheme: %v", err)
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()

	return NewChartService(c, scheme, testNamespace), c
}

// TestEnsureHelmChartForcesReconcileFromAddon covers the force reconcile
// annotation applied to the HelmClusterAddon: on the internal Helm repository
// path it must reach the HelmChart, so that a forced addon re-pulls its source
// instead of only nudging the HelmRelease.
func TestEnsureHelmChartForcesReconcileFromAddon(t *testing.T) {
	addon := testAddon()
	addon.Annotations = map[string]string{helmv1alpha1.AnnotationForceReconcile: "2026-01-01T00:00:00Z"}
	service, c := newChartService(t, addon)

	service.EnsureHelmChart(context.Background(), addon)

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

// TestEnsureHelmChartDoesNotForceReconcileWithoutAnnotation is the complement:
// an unannotated addon must not stamp a fresh timestamp on every pass, which
// would make the source controller re-reconcile continuously.
func TestEnsureHelmChartDoesNotForceReconcileWithoutAnnotation(t *testing.T) {
	addon := testAddon()
	service, c := newChartService(t, addon)

	service.EnsureHelmChart(context.Background(), addon)

	chart := &sourcev1.HelmChart{}
	key := client.ObjectKey{Name: utils.GetInternalHelmChartName(addon.Name), Namespace: testNamespace}
	if err := c.Get(context.Background(), key, chart); err != nil {
		t.Fatalf("helm chart was not created: %v", err)
	}

	if _, found := chart.Annotations[meta.ReconcileRequestAnnotation]; found {
		t.Errorf("%s must not be stamped without a force request", meta.ReconcileRequestAnnotation)
	}
}
