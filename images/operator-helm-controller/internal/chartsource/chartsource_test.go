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

package chartsource

import (
	"testing"

	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
)

func TestResolve(t *testing.T) {
	helmRepo := &helmv1alpha1.HelmClusterAddonRepository{
		Spec: helmv1alpha1.RepositorySpec{URL: "https://charts.example.com/stable"},
	}
	ociRepo := &helmv1alpha1.HelmClusterAddonRepository{
		Spec: helmv1alpha1.RepositorySpec{URL: "oci://registry.example.com/charts/podinfo"},
	}

	tests := []struct {
		name    string
		repo    *helmv1alpha1.HelmClusterAddonRepository
		version helmv1alpha1.ChartVersion
		want    Source
		wantErr bool
	}{
		{
			// The whole point of the feature: the index entry decides, not the
			// repository scheme.
			name: "index entry pointing at a registry wins over the repository scheme",
			repo: helmRepo,
			version: helmv1alpha1.ChartVersion{
				Version: "25.0.2",
				OCIRef:  "oci://registry-1.docker.io/bitnamicharts/airflow:25.0.2",
			},
			want: Source{
				Kind: OCI,
				URL:  "oci://registry-1.docker.io/bitnamicharts/airflow",
				Tag:  "25.0.2",
			},
		},
		{
			name:    "helm repository without an oci reference stays on the helm path",
			repo:    helmRepo,
			version: helmv1alpha1.ChartVersion{Version: "6.7.1"},
			want:    Source{Kind: Helm},
		},
		{
			name:    "oci repository addresses its own url at the version tag",
			repo:    ociRepo,
			version: helmv1alpha1.ChartVersion{Version: "6.7.1", MediaType: "application/tar+gzip"},
			want: Source{
				Kind: OCI,
				URL:  "oci://registry.example.com/charts/podinfo",
				Tag:  "6.7.1",
			},
		},
		{
			name:    "unparsable recorded reference is an error",
			repo:    helmRepo,
			version: helmv1alpha1.ChartVersion{Version: "1.0.0", OCIRef: "oci://BAD_HOST//:::"},
			wantErr: true,
		},
		{
			name: "unsupported repository scheme is an error",
			repo: &helmv1alpha1.HelmClusterAddonRepository{
				Spec: helmv1alpha1.RepositorySpec{URL: "ftp://charts.example.com"},
			},
			version: helmv1alpha1.ChartVersion{Version: "1.0.0"},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Resolve(tt.repo.Spec.URL, &tt.version)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got %+v", got)
				}

				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("source = %+v, want %+v", got, tt.want)
			}
		})
	}
}
