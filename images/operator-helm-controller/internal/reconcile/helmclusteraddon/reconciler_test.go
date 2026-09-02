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

package helmclusteraddon

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/deckhouse/operator-helm/api/naming"
	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
	"github.com/deckhouse/operator-helm/internal/utils"
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("registering client-go scheme: %v", err)
	}
	if err := helmv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("registering helm scheme: %v", err)
	}

	return scheme
}

func newTestReconciler(t *testing.T, objects ...client.Object) *Reconciler {
	t.Helper()

	scheme := testScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()

	return &Reconciler{Client: c}
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

func addonChartFixture(repoName, chartName string, versions ...helmv1alpha1.HelmClusterAddonChartVersion) *helmv1alpha1.HelmClusterAddonChart {
	return &helmv1alpha1.HelmClusterAddonChart{
		ObjectMeta: metav1.ObjectMeta{
			Name: naming.HelmClusterAddonChartName(repoName, chartName),
		},
		Status: helmv1alpha1.HelmClusterAddonChartStatus{Versions: versions},
	}
}

// TestGetHelmClusterAddonChart pins the gate that decides whether an addon has
// enough information to be deployed. For an OCI repository, a catalog entry is
// usable exactly when it carries a media type; for a Helm repository the media
// type is never checked, so an entry is usable as soon as the version is present.
func TestGetHelmClusterAddonChart(t *testing.T) {
	addon := testAddon()

	tests := []struct {
		name string
		// version is the sole entry seeded into the HelmClusterAddonChart's
		// Status.Versions. Its own Version field decides whether the lookup by
		// addon.Spec.Chart.Version ("6.7.1") hits or misses.
		version        helmv1alpha1.HelmClusterAddonChartVersion
		repoType       utils.InternalRepositoryType
		wantErr        bool
		wantErrContain string
	}{
		{
			name: "oci version with a media type passes",
			version: helmv1alpha1.HelmClusterAddonChartVersion{
				Version:   "6.7.1",
				MediaType: "application/vnd.cncf.helm.chart.content.v1.tar+gzip",
			},
			repoType: utils.InternalOCIRepository,
		},
		{
			// Deliberate: the tag disappeared from the repository, but the entry is
			// retained with its media type so the addon keeps reconciling everything
			// else. The real pull failure is reported by the source controller.
			name: "oci version removed from repository but with a media type still passes",
			version: helmv1alpha1.HelmClusterAddonChartVersion{
				Version:           "6.7.1",
				MediaType:         "application/tar+gzip",
				UnavailableReason: helmv1alpha1.UnavailableReasonRemovedFromRepository,
			},
			repoType: utils.InternalOCIRepository,
		},
		{
			name: "oci version stuck resolving is rejected with reason and message",
			version: helmv1alpha1.HelmClusterAddonChartVersion{
				Version:            "6.7.1",
				UnavailableReason:  helmv1alpha1.UnavailableReasonResolvePending,
				UnavailableMessage: "manifest request failed",
			},
			repoType:       utils.InternalOCIRepository,
			wantErr:        true,
			wantErrContain: "ResolvePending: manifest request failed",
		},
		{
			name: "oci version with unsupported media type and no message is rejected with reason alone",
			version: helmv1alpha1.HelmClusterAddonChartVersion{
				Version:           "6.7.1",
				UnavailableReason: helmv1alpha1.UnavailableReasonUnsupportedMediaType,
			},
			repoType:       utils.InternalOCIRepository,
			wantErr:        true,
			wantErrContain: "UnsupportedMediaType",
		},
		{
			// Same empty-media-type entry as above, but a Helm repository's versions
			// never carry a media type: a stricter gate here would break every Helm
			// addon, so the presence check alone must let it through.
			name: "the same empty media type entry passes for a helm repository",
			version: helmv1alpha1.HelmClusterAddonChartVersion{
				Version:           "6.7.1",
				UnavailableReason: helmv1alpha1.UnavailableReasonUnsupportedMediaType,
			},
			repoType: utils.InternalHelmRepository,
		},
		{
			name:           "a version the addon does not reference is rejected",
			version:        helmv1alpha1.HelmClusterAddonChartVersion{Version: "9.9.9"},
			repoType:       utils.InternalOCIRepository,
			wantErr:        true,
			wantErrContain: `does not have version "6.7.1"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			chart := addonChartFixture(
				addon.Spec.Chart.HelmClusterAddonRepository, addon.Spec.Chart.HelmClusterAddonChartName, tt.version,
			)
			r := newTestReconciler(t, chart)

			gotChart, gotVersion, err := r.getHelmClusterAddonChart(context.Background(), addon, tt.repoType)

			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got version %+v", gotVersion)
				}
				if !strings.Contains(err.Error(), tt.wantErrContain) {
					t.Fatalf("error %q does not contain %q", err.Error(), tt.wantErrContain)
				}
				if gotChart != nil || gotVersion != nil {
					t.Fatalf("expected nil chart and version on error, got chart=%v version=%v", gotChart, gotVersion)
				}

				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if gotChart == nil {
				t.Fatal("expected the chart to be returned")
			}
			if gotVersion == nil {
				t.Fatal("expected the matched version to be returned")
			}
			if gotVersion.Version != tt.version.Version {
				t.Fatalf("returned version = %q, want %q", gotVersion.Version, tt.version.Version)
			}
			if gotVersion.MediaType != tt.version.MediaType {
				t.Fatalf("returned version media type = %q, want %q", gotVersion.MediaType, tt.version.MediaType)
			}
		})
	}
}

func TestGetHelmClusterAddonChartMissingChart(t *testing.T) {
	addon := testAddon()
	r := newTestReconciler(t)

	gotChart, gotVersion, err := r.getHelmClusterAddonChart(context.Background(), addon, utils.InternalOCIRepository)
	if err == nil {
		t.Fatalf("expected an error when the addon chart does not exist, got version %+v", gotVersion)
	}
	if gotChart != nil || gotVersion != nil {
		t.Fatalf("expected nil chart and version on error, got chart=%v version=%v", gotChart, gotVersion)
	}
}
