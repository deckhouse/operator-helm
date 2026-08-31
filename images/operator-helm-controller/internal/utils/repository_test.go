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
