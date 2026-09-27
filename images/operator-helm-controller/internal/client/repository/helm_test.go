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
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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

const testHybridIndex = `apiVersion: v1
entries:
  airflow:
    - version: 25.0.2
      urls:
        - oci://registry-1.docker.io/bitnamicharts/airflow:25.0.2
    - version: 24.0.0
      urls:
        - https://charts.example.invalid/airflow-24.0.0.tgz
        - oci://registry-1.docker.io/bitnamicharts/airflow:24.0.0
    - version: 23.0.0
      urls:
        - oci://registry-1.docker.io/bitnamicharts/airflow
    - version: 22.0.0
      urls:
        - "oci://BAD_HOST//:::"
    - version: 21.0.0
`

// shortenIndexLimits shrinks the fetch limits for the duration of one test, so the
// behaviour they guard can be exercised without waiting out real minutes or
// streaming real megabytes.
func shortenIndexLimits(t *testing.T, attempt, budget time.Duration, size int) {
	t.Helper()

	attemptBefore, budgetBefore, sizeBefore := indexAttemptTimeout, indexFetchBudget, maxIndexSize

	indexAttemptTimeout, indexFetchBudget, maxIndexSize = attempt, budget, size

	t.Cleanup(func() {
		indexAttemptTimeout, indexFetchBudget, maxIndexSize = attemptBefore, budgetBefore, sizeBefore
	})
}

// TestFetchChartsRetriesAfterAnAttemptStalls pins that a stalled attempt costs only
// its own deadline and not the whole budget. The first request never answers, so
// with a single deadline shared by all attempts the fetch would end with that one
// request and report a timeout; the second request proves the retry still had time
// of its own.
func TestFetchChartsRetriesAfterAnAttemptStalls(t *testing.T) {
	shortenIndexLimits(t, 200*time.Millisecond, 10*time.Second, maxIndexSize)

	var requests atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == 1 {
			// Hang until the attempt gives up on its own deadline.
			<-r.Context().Done()

			return
		}

		_, _ = w.Write([]byte(testIndex))
	}))
	defer srv.Close()

	charts, err := HelmRepositoryDefaultClient.FetchCharts(context.Background(), srv.URL, nil, FetchOptions{})
	if err != nil {
		t.Fatalf("expected the second attempt to succeed, got %v", err)
	}
	if got := requests.Load(); got < 2 {
		t.Fatalf("expected the stalled attempt to be retried, got %d request(s)", got)
	}
	if len(charts) != 1 {
		t.Fatalf("expected the index of the second attempt to be decoded, got %d charts", len(charts))
	}
}

// TestFetchChartsRejectsAnOversizeIndex pins that the response body is bounded. The
// served document is valid YAML all the way through, so an unbounded read would
// either hold all of it in memory or — once cut at a limit — decode the truncated
// prefix as a smaller but perfectly valid repository and prune the rest of the
// catalog. The fetch must fail instead, saying why, and must not retry a download
// whose size will not change.
func TestFetchChartsRejectsAnOversizeIndex(t *testing.T) {
	const limit = 1 << 20

	shortenIndexLimits(t, indexAttemptTimeout, indexFetchBudget, limit)

	var requests atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)

		_, _ = w.Write([]byte("apiVersion: v1\nentries:\n"))

		// Comfortably past the limit, one well-formed entry at a time.
		for i := range 40000 {
			_, _ = fmt.Fprintf(w, "  chart-%d:\n    - version: 1.0.0\n", i)
		}
	}))
	defer srv.Close()

	charts, err := HelmRepositoryDefaultClient.FetchCharts(context.Background(), srv.URL, nil, FetchOptions{})
	if err == nil {
		t.Fatalf("expected an oversize index to fail, got %d charts", len(charts))
	}
	if charts != nil {
		t.Fatalf("expected no charts alongside the error, got %d", len(charts))
	}
	if !strings.Contains(err.Error(), "too large") || !strings.Contains(err.Error(), "1 MiB") {
		t.Fatalf("expected the error to name the size limit, got %v", err)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("an oversize index must not be downloaded again, got %d request(s)", got)
	}
}

// TestFetchChartsAcceptsAnIndexAtTheLimit pins the boundary: the limit is on the
// document, not on what fits under it, so an index of exactly maxIndexSize bytes is
// still a good index.
func TestFetchChartsAcceptsAnIndexAtTheLimit(t *testing.T) {
	index := testIndex + strings.Repeat("# pad\n", 4)

	shortenIndexLimits(t, indexAttemptTimeout, indexFetchBudget, len(index))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(index))
	}))
	defer srv.Close()

	charts, err := HelmRepositoryDefaultClient.FetchCharts(context.Background(), srv.URL, nil, FetchOptions{})
	if err != nil {
		t.Fatalf("an index of exactly the limit must be accepted, got %v", err)
	}
	if len(charts) != 1 {
		t.Fatalf("expected 1 chart, got %d", len(charts))
	}
}

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

			_, err := HelmRepositoryDefaultClient.FetchCharts(context.Background(), srv.URL, nil, FetchOptions{})
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

	_, err := HelmRepositoryDefaultClient.FetchCharts(context.Background(), srv.URL, nil, FetchOptions{})
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

	charts, err := HelmRepositoryDefaultClient.FetchCharts(context.Background(), srv.URL, nil, FetchOptions{})
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

// TestFetchChartsHelmRecordsOCIReferences pins how an index entry is classified. Only
// urls[0] is examined, because the internal HelmChart downloads exactly that url:
// picking an archive from urls[1] would send the version down the helm path where the
// source controller would still trip over urls[0].
func TestFetchChartsHelmRecordsOCIReferences(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(testHybridIndex))
	}))
	t.Cleanup(server.Close)

	charts, err := HelmRepositoryDefaultClient.FetchCharts(
		context.Background(), server.URL+"/index.yaml", nil, FetchOptions{},
	)
	if err != nil {
		t.Fatalf("FetchCharts returned %v", err)
	}
	if len(charts) != 1 {
		t.Fatalf("expected one chart, got %d", len(charts))
	}

	byVersion := map[string]ChartVersion{}
	for _, version := range charts[0].Versions {
		byVersion[version.Version.Original()] = version
	}

	if got := byVersion["25.0.2"].OCIRef; got != "oci://registry-1.docker.io/bitnamicharts/airflow:25.0.2" {
		t.Fatalf("25.0.2 oci ref = %q, want the reference from the index", got)
	}

	if got := byVersion["24.0.0"].OCIRef; got != "" {
		t.Fatalf("24.0.0 oci ref = %q, want empty: its first url is an archive", got)
	}

	// The tag is made explicit on write, so the recorded reference is self-contained.
	if got := byVersion["23.0.0"].OCIRef; got != "oci://registry-1.docker.io/bitnamicharts/airflow:23.0.0" {
		t.Fatalf("23.0.0 oci ref = %q, want the entry version as the tag", got)
	}

	invalid := byVersion["22.0.0"]
	if invalid.OCIRef != "" {
		t.Fatalf("22.0.0 oci ref = %q, want empty for an unusable reference", invalid.OCIRef)
	}
	if invalid.UnavailableReason != helmv1alpha1.UnavailableReasonInvalidChartReference {
		t.Fatalf("22.0.0 reason = %q, want %q", invalid.UnavailableReason, helmv1alpha1.UnavailableReasonInvalidChartReference)
	}
	if invalid.UnavailableMessage == "" {
		t.Fatal("an unusable reference must be explained in the message")
	}

	if got := byVersion["21.0.0"].OCIRef; got != "" {
		t.Fatalf("21.0.0 oci ref = %q, want empty: the entry has no urls", got)
	}
}

// TestFetchChartsRejectsAResponseWithoutAnIndex pins that a response carrying no
// index document is a failed fetch. yaml accepts every one of these bodies without
// an error and leaves the entries mapping absent, so reporting success would hand
// the caller an empty chart list and prune the repository's whole catalog — over a
// proxy that answered 200 with nothing, a 204 (which is not a terminal status), or
// an error page. The failure must stay retriable: the repository resource is fine.
func TestFetchChartsRejectsAResponseWithoutAnIndex(t *testing.T) {
	cases := []struct {
		name       string
		statusCode int
		body       string
	}{
		{"empty body with 200", http.StatusOK, ""},
		{"no content", http.StatusNoContent, ""},
		{"blank document", http.StatusOK, "\n  \n"},
		{"error page instead of the index", http.StatusOK, `{"error":"upstream unavailable"}`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var requests atomic.Int32

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests.Add(1)

				w.WriteHeader(tc.statusCode)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			charts, err := HelmRepositoryDefaultClient.FetchCharts(context.Background(), srv.URL, nil, FetchOptions{})
			if err == nil {
				t.Fatalf("expected a response without an index to fail, got %d charts", len(charts))
			}
			if charts != nil {
				t.Fatalf("expected no charts alongside the error, got %d", len(charts))
			}
			if _, ok := AsTerminal(err); ok {
				t.Fatalf("a missing index document must stay retriable, got terminal error: %v", err)
			}
			if !strings.Contains(err.Error(), "no index document") {
				t.Fatalf("expected the reported cause to name the missing index, got %v", err)
			}
			if got := requests.Load(); got < 2 {
				t.Fatalf("expected the fetch to be retried, got %d request(s)", got)
			}
		})
	}
}

// TestFetchChartsAcceptsAnIndexWithoutCharts is the guard on the check above: a
// repository is allowed to publish an index that lists nothing, and that is a
// successful fetch of zero charts — the catalog reconciler then prunes correctly,
// because the repository really did go empty.
func TestFetchChartsAcceptsAnIndexWithoutCharts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("apiVersion: v1\nentries: {}\n"))
	}))
	defer srv.Close()

	charts, err := HelmRepositoryDefaultClient.FetchCharts(context.Background(), srv.URL, nil, FetchOptions{})
	if err != nil {
		t.Fatalf("an index that lists no charts must be accepted, got %v", err)
	}
	// Non-nil and empty: the caller distinguishes nothing here, it prunes whatever the
	// catalog holds beyond this list.
	if charts == nil {
		t.Fatal("expected an empty chart list, got nil")
	}
	if len(charts) != 0 {
		t.Fatalf("expected no charts, got %d", len(charts))
	}
}

// TestFetchChartsThrottleIsRetriedNotDecoded pins that a throttled read is never
// mistaken for an empty repository. A throttle is deliberately not terminal, and the
// body registries send with it is JSON, which decodes into an empty index without
// error — so letting it reach the decoder would report a successful fetch of zero
// charts and prune the repository's whole catalog.
func TestFetchChartsThrottleIsRetriedNotDecoded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"errors":[{"code":"TOOMANYREQUESTS","message":"pull rate limit exceeded"}]}`))
	}))
	defer srv.Close()

	charts, err := HelmRepositoryDefaultClient.FetchCharts(context.Background(), srv.URL, nil, FetchOptions{})
	if err == nil {
		t.Fatalf("expected an error for repeated 429 responses, got %d charts", len(charts))
	}
	if _, ok := AsTerminal(err); ok {
		t.Fatalf("a throttle must stay retriable, got terminal error: %v", err)
	}
	if !strings.Contains(err.Error(), "429") {
		t.Fatalf("expected the status code in the reported cause, got %v", err)
	}
}
