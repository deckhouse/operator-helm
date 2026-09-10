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

package utils

import (
	"encoding/base64"
	"encoding/json"
	"testing"

	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
)

func TestGetRegistryHost(t *testing.T) {
	tests := []struct {
		url     string
		want    string
		wantErr bool
	}{
		{url: "oci://registry.example.com/charts/podinfo", want: "registry.example.com"},
		{url: "oci://registry.example.com:5000/charts", want: "registry.example.com:5000"},
		{url: "https://charts.example.com/stable", want: "charts.example.com"},
		{url: "oci:///charts", wantErr: true},
	}

	for _, tt := range tests {
		got, err := GetRegistryHost(tt.url)
		if tt.wantErr {
			if err == nil {
				t.Errorf("GetRegistryHost(%q): expected error, got %q", tt.url, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("GetRegistryHost(%q): unexpected error: %v", tt.url, err)
			continue
		}
		if got != tt.want {
			t.Errorf("GetRegistryHost(%q) = %q, want %q", tt.url, got, tt.want)
		}
	}
}

func TestBuildDockerConfigJSON(t *testing.T) {
	encoded, err := BuildDockerConfigJSON("oci://registry.example.com:5000/charts/podinfo", "user", "pass")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var config struct {
		Auths map[string]struct {
			Username string `json:"username"`
			Password string `json:"password"`
			Auth     string `json:"auth"`
		} `json:"auths"`
	}
	if err := json.Unmarshal([]byte(encoded), &config); err != nil {
		t.Fatalf("cannot unmarshal docker config %q: %v", encoded, err)
	}

	entry, ok := config.Auths["registry.example.com:5000"]
	if !ok {
		t.Fatalf("expected credentials keyed by registry host, got %v", config.Auths)
	}
	if entry.Username != "user" || entry.Password != "pass" {
		t.Errorf("unexpected credentials: %+v", entry)
	}
	if want := base64.StdEncoding.EncodeToString([]byte("user:pass")); entry.Auth != want {
		t.Errorf("auth = %q, want %q", entry.Auth, want)
	}
}

func TestBuildDockerConfigJSONInvalidURL(t *testing.T) {
	if _, err := BuildDockerConfigJSON("not-a-url", "user", "pass"); err == nil {
		t.Fatal("expected error for url without host")
	}
}

func TestBuildDockerConfigJSONDockerHub(t *testing.T) {
	encoded, err := BuildDockerConfigJSON("oci://docker.io/charts/podinfo", "user", "pass")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var config struct {
		Auths map[string]json.RawMessage `json:"auths"`
	}
	if err := json.Unmarshal([]byte(encoded), &config); err != nil {
		t.Fatalf("cannot unmarshal docker config %q: %v", encoded, err)
	}

	for _, key := range []string{"https://index.docker.io/v1/", "index.docker.io", "docker.io"} {
		if _, ok := config.Auths[key]; !ok {
			t.Errorf("expected credentials under %q, got keys %v", key, config.Auths)
		}
	}
}

func TestSplitOCIRef(t *testing.T) {
	tests := []struct {
		name     string
		ref      string
		fallback string
		wantURL  string
		wantTag  string
		wantErr  bool
	}{
		{
			name:     "explicit tag",
			ref:      "oci://registry-1.docker.io/bitnamicharts/airflow:25.0.2",
			fallback: "25.0.2",
			wantURL:  "oci://registry-1.docker.io/bitnamicharts/airflow",
			wantTag:  "25.0.2",
		},
		{
			name:     "no tag falls back to the index entry version",
			ref:      "oci://registry-1.docker.io/bitnamicharts/airflow",
			fallback: "25.0.2",
			wantURL:  "oci://registry-1.docker.io/bitnamicharts/airflow",
			wantTag:  "25.0.2",
		},
		{
			name:     "registry port is not mistaken for a tag",
			ref:      "oci://registry.example.com:5000/charts/podinfo",
			fallback: "6.7.1",
			wantURL:  "oci://registry.example.com:5000/charts/podinfo",
			wantTag:  "6.7.1",
		},
		{
			name:     "port and tag together",
			ref:      "oci://registry.example.com:5000/charts/podinfo:6.7.1",
			fallback: "6.7.1",
			wantURL:  "oci://registry.example.com:5000/charts/podinfo",
			wantTag:  "6.7.1",
		},
		{
			// The address must keep the spelling of the index: go-containerregistry
			// rewrites docker.io to index.docker.io, and the internal OCIRepository
			// has to address the registry the repository actually named.
			name:     "docker.io is not rewritten",
			ref:      "oci://docker.io/bitnamicharts/airflow:25.0.2",
			fallback: "25.0.2",
			wantURL:  "oci://docker.io/bitnamicharts/airflow",
			wantTag:  "25.0.2",
		},
		{
			name:     "digest reference is rejected",
			ref:      "oci://registry.example.com/charts/podinfo@sha256:0000000000000000000000000000000000000000000000000000000000000000",
			fallback: "6.7.1",
			wantErr:  true,
		},
		{
			name:     "reference without a chart path is rejected",
			ref:      "oci://registry.example.com:5000",
			fallback: "6.7.1",
			wantErr:  true,
		},
		{
			name:     "garbage is rejected",
			ref:      "oci://BAD_HOST//:::",
			fallback: "6.7.1",
			wantErr:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotURL, gotTag, err := SplitOCIRef(tt.ref, tt.fallback)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got %q %q", gotURL, gotTag)
				}

				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if gotURL != tt.wantURL {
				t.Fatalf("url = %q, want %q", gotURL, tt.wantURL)
			}
			if gotTag != tt.wantTag {
				t.Fatalf("tag = %q, want %q", gotTag, tt.wantTag)
			}
		})
	}
}

func TestResolveChartSource(t *testing.T) {
	helmRepo := &helmv1alpha1.HelmClusterAddonRepository{
		Spec: helmv1alpha1.RepositorySpec{URL: "https://charts.example.com/stable"},
	}
	ociRepo := &helmv1alpha1.HelmClusterAddonRepository{
		Spec: helmv1alpha1.RepositorySpec{URL: "oci://registry.example.com/charts/podinfo"},
	}

	tests := []struct {
		name    string
		repo    *helmv1alpha1.HelmClusterAddonRepository
		version helmv1alpha1.HelmClusterAddonChartVersion
		want    ChartSource
		wantErr bool
	}{
		{
			// The whole point of the feature: the index entry decides, not the
			// repository scheme.
			name: "index entry pointing at a registry wins over the repository scheme",
			repo: helmRepo,
			version: helmv1alpha1.HelmClusterAddonChartVersion{
				Version: "25.0.2",
				OCIRef:  "oci://registry-1.docker.io/bitnamicharts/airflow:25.0.2",
			},
			want: ChartSource{
				Kind: InternalOCIRepository,
				URL:  "oci://registry-1.docker.io/bitnamicharts/airflow",
				Tag:  "25.0.2",
			},
		},
		{
			name:    "helm repository without an oci reference stays on the helm path",
			repo:    helmRepo,
			version: helmv1alpha1.HelmClusterAddonChartVersion{Version: "6.7.1"},
			want:    ChartSource{Kind: InternalHelmRepository},
		},
		{
			name:    "oci repository addresses its own url at the version tag",
			repo:    ociRepo,
			version: helmv1alpha1.HelmClusterAddonChartVersion{Version: "6.7.1", MediaType: "application/tar+gzip"},
			want: ChartSource{
				Kind: InternalOCIRepository,
				URL:  "oci://registry.example.com/charts/podinfo",
				Tag:  "6.7.1",
			},
		},
		{
			name:    "unparsable recorded reference is an error",
			repo:    helmRepo,
			version: helmv1alpha1.HelmClusterAddonChartVersion{Version: "1.0.0", OCIRef: "oci://BAD_HOST//:::"},
			wantErr: true,
		},
		{
			name: "unsupported repository scheme is an error",
			repo: &helmv1alpha1.HelmClusterAddonRepository{
				Spec: helmv1alpha1.RepositorySpec{URL: "ftp://charts.example.com"},
			},
			version: helmv1alpha1.HelmClusterAddonChartVersion{Version: "1.0.0"},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ResolveChartSource(tt.repo, &tt.version)
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
