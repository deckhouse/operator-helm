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

package repository

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
)

const testIndex = `apiVersion: v1
entries:
  podinfo:
    - version: 6.7.1
      icon: https://example.invalid/icon.png
    - version: not-a-semver
    - version: 6.7.0
`

func TestFetchChartsTerminalStatusCodes(t *testing.T) {
	cases := []struct {
		name       string
		statusCode int
		wantReason string
	}{
		{"unauthorized", http.StatusUnauthorized, helmv1alpha1.ReasonAuthenticationFailed},
		{"forbidden", http.StatusForbidden, helmv1alpha1.ReasonAuthenticationFailed},
		{"not found", http.StatusNotFound, helmv1alpha1.ReasonSourceNotFound},
		{"teapot", http.StatusTeapot, helmv1alpha1.ReasonSourceRejectedRequest},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.statusCode)
			}))
			defer srv.Close()

			_, err := HelmRepositoryDefaultClient.FetchCharts(context.Background(), srv.URL, nil)
			if err == nil {
				t.Fatalf("expected error for status %d", tc.statusCode)
			}

			terminal, ok := AsTerminal(err)
			if !ok {
				t.Fatalf("expected terminal error, got %v", err)
			}
			if terminal.Reason != tc.wantReason {
				t.Fatalf("expected reason %q, got %q", tc.wantReason, terminal.Reason)
			}
		})
	}
}

func TestFetchChartsServerErrorIsNotTerminal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	_, err := HelmRepositoryDefaultClient.FetchCharts(context.Background(), srv.URL, nil)
	if err == nil {
		t.Fatal("expected error for repeated 500 responses")
	}
	if _, ok := AsTerminal(err); ok {
		t.Fatalf("5xx must stay retriable, got terminal error: %v", err)
	}
	// The exhausted backoff must report what actually went wrong rather than the
	// bare "timed out waiting for the condition" the wait helper returns.
	if !strings.Contains(err.Error(), "500") {
		t.Fatalf("expected the status code in the reported cause, got %v", err)
	}
}

func TestFetchChartsSkipsInvalidVersion(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(testIndex))
	}))
	defer srv.Close()

	charts, err := HelmRepositoryDefaultClient.FetchCharts(context.Background(), srv.URL, nil)
	if err != nil {
		t.Fatalf("expected invalid version to be skipped, got error: %v", err)
	}
	if len(charts) != 1 {
		t.Fatalf("expected 1 chart, got %d", len(charts))
	}
	if len(charts[0].Versions) != 2 {
		t.Fatalf("expected 2 valid versions, got %d", len(charts[0].Versions))
	}
	if got := charts[0].Versions[0].Version.Original(); got != "6.7.1" {
		t.Fatalf("expected newest version first, got %q", got)
	}
}
