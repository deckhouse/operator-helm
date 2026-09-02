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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	sourcev1 "github.com/werf/nelm-source-controller/api/v1"

	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
	"github.com/deckhouse/operator-helm/internal/utils"
)

func newOCIRepoService(t *testing.T, objects ...client.Object) (*OCIRepoService, client.Client) {
	t.Helper()

	scheme := testScheme(t)
	if err := sourcev1.AddToScheme(scheme); err != nil {
		t.Fatalf("registering source scheme: %v", err)
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()

	return NewOCIRepoService(c, scheme, testNamespace), c
}

func testAddon() *helmv1alpha1.HelmClusterAddon {
	return &helmv1alpha1.HelmClusterAddon{
		ObjectMeta: metav1.ObjectMeta{Name: "consumer", Generation: 1},
		Spec: helmv1alpha1.HelmClusterAddonSpec{
			Namespace: "app",
			Chart: helmv1alpha1.HelmClusterAddonChartRef{
				HelmClusterAddonRepository: "example",
				HelmClusterAddonChartName:  "podinfo",
				Version:                    "6.7.1",
			},
		},
	}
}

func ociTestRepository() *helmv1alpha1.HelmClusterAddonRepository {
	return &helmv1alpha1.HelmClusterAddonRepository{
		ObjectMeta: metav1.ObjectMeta{Name: "example", Generation: 1},
		Spec:       helmv1alpha1.HelmClusterAddonRepositorySpec{URL: "oci://example.invalid/podinfo"},
	}
}

func TestEnsureInternalOCIRepositoryUsesRecordedMediaType(t *testing.T) {
	addon, repo := testAddon(), ociTestRepository()
	service, c := newOCIRepoService(t, addon, repo)

	version := &helmv1alpha1.HelmClusterAddonChartVersion{
		Version:   "6.7.1",
		MediaType: "application/tar+gzip",
	}

	service.EnsureInternalOCIRepository(context.Background(), addon, repo, version)

	ociRepo := &sourcev1.OCIRepository{}
	key := client.ObjectKey{Name: utils.GetInternalOCIRepositoryName(addon.Name), Namespace: testNamespace}
	if err := c.Get(context.Background(), key, ociRepo); err != nil {
		t.Fatalf("oci repository was not created: %v", err)
	}

	if ociRepo.Spec.LayerSelector == nil {
		t.Fatal("layer selector must be set")
	}
	if ociRepo.Spec.LayerSelector.MediaType != "application/tar+gzip" {
		t.Fatalf("layer media type is %q, want the recorded legacy one", ociRepo.Spec.LayerSelector.MediaType)
	}
}

func TestEnsureInternalOCIRepositoryReportsRemovedVersion(t *testing.T) {
	addon, repo := testAddon(), ociTestRepository()
	service, _ := newOCIRepoService(t, addon, repo)

	version := &helmv1alpha1.HelmClusterAddonChartVersion{
		Version:           "6.7.1",
		MediaType:         "application/tar+gzip",
		UnavailableReason: helmv1alpha1.UnavailableReasonRemovedFromRepository,
	}

	result := service.EnsureInternalOCIRepository(context.Background(), addon, repo, version)

	if result.Status.Reason != helmv1alpha1.ReasonChartVersionRemoved {
		t.Fatalf("reason is %q, want %q", result.Status.Reason, helmv1alpha1.ReasonChartVersionRemoved)
	}
	if result.Status.Message == "" {
		t.Fatal("a removed version must be explained in the message")
	}
}
