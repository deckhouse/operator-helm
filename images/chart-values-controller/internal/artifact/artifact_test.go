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

package artifact

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func buildChartArchive(t *testing.T, files map[string]string) []byte {
	t.Helper()

	entries := make([]archiveEntry, 0, len(files))
	for name, content := range files {
		entries = append(entries, archiveEntry{name: name, content: content})
	}

	return buildOrderedChartArchive(t, entries)
}

type archiveEntry struct {
	name    string
	content string
}

// buildOrderedChartArchive writes the entries in the order given. Order is what a
// budget test turns on: values.yaml ends the walk, so a payload that must be
// walked has to be written before it.
func buildOrderedChartArchive(t *testing.T, entries []archiveEntry) []byte {
	t.Helper()

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	for _, entry := range entries {
		if err := tw.WriteHeader(&tar.Header{
			Name:     entry.name,
			Mode:     0o644,
			Size:     int64(len(entry.content)),
			Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatalf("writing tar header: %v", err)
		}
		if _, err := tw.Write([]byte(entry.content)); err != nil {
			t.Fatalf("writing tar content: %v", err)
		}
	}

	if err := tw.Close(); err != nil {
		t.Fatalf("closing tar: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("closing gzip: %v", err)
	}

	return buf.Bytes()
}

// buildArchiveWithEntries writes count header-only entries, so the archive is
// judged by how many files it claims rather than by how much they hold.
func buildArchiveWithEntries(t *testing.T, count int) []byte {
	t.Helper()

	entries := make([]archiveEntry, 0, count+1)
	for i := range count {
		entries = append(entries, archiveEntry{name: fmt.Sprintf("podinfo/templates/entry-%d.yaml", i)})
	}
	entries = append(entries, archiveEntry{name: "podinfo/values.yaml", content: "a: b\n"})

	return buildOrderedChartArchive(t, entries)
}

func digestOf(data []byte) string {
	return fmt.Sprintf("sha256:%x", sha256.Sum256(data))
}

func serve(t *testing.T, data []byte) string {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(data)
	}))
	t.Cleanup(srv.Close)

	return srv.URL
}

const testMaxBytes = 10 << 20

func TestFetchValuesSuccess(t *testing.T) {
	want := "replicaCount: 1\nimage:\n  tag: latest\n"
	archive := buildChartArchive(t, map[string]string{
		"podinfo/Chart.yaml":  "name: podinfo\n",
		"podinfo/values.yaml": want,
	})

	url := serve(t, archive)

	got, err := FetchValues(context.Background(), http.DefaultClient, url, digestOf(archive), testMaxBytes)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(got) != want {
		t.Fatalf("values mismatch:\ngot:  %q\nwant: %q", got, want)
	}
}

func TestFetchValuesMissing(t *testing.T) {
	archive := buildChartArchive(t, map[string]string{
		"podinfo/Chart.yaml": "name: podinfo\n",
	})

	url := serve(t, archive)

	_, err := FetchValues(context.Background(), http.DefaultClient, url, digestOf(archive), testMaxBytes)
	if !errors.Is(err, ErrValuesNotFound) {
		t.Fatalf("expected ErrValuesNotFound, got %v", err)
	}
}

func TestFetchValuesNestedValuesIgnored(t *testing.T) {
	// values.yaml of a subchart must not be mistaken for the chart's own.
	archive := buildChartArchive(t, map[string]string{
		"podinfo/charts/dep/values.yaml": "nested: true\n",
	})

	url := serve(t, archive)

	_, err := FetchValues(context.Background(), http.DefaultClient, url, digestOf(archive), testMaxBytes)
	if !errors.Is(err, ErrValuesNotFound) {
		t.Fatalf("expected ErrValuesNotFound for nested-only values, got %v", err)
	}
}

func TestFetchValuesDigestMismatch(t *testing.T) {
	archive := buildChartArchive(t, map[string]string{
		"podinfo/values.yaml": "a: b\n",
	})

	url := serve(t, archive)

	_, err := FetchValues(context.Background(), http.DefaultClient, url, "sha256:deadbeef", testMaxBytes)
	if err == nil || errors.Is(err, ErrValuesNotFound) {
		t.Fatalf("expected digest mismatch error, got %v", err)
	}
}

func TestFetchValuesTooLarge(t *testing.T) {
	archive := buildChartArchive(t, map[string]string{
		"podinfo/values.yaml": "a: b\n",
	})

	url := serve(t, archive)

	// A limit smaller than the archive must be rejected before digest checks.
	_, err := FetchValues(context.Background(), http.DefaultClient, url, digestOf(archive), 8)
	if !errors.Is(err, ErrArtifactTooLarge) {
		t.Fatalf("expected ErrArtifactTooLarge, got %v", err)
	}
}

// TestExtractValuesRejectsAnArchiveThatExpandsPastTheTotalBudget covers the
// decompression bomb the per-entry bound left through: the values.yaml of such an
// archive is small and well within every per-file limit, while the entries walked
// on the way to it expand into orders of magnitude more than the archive itself.
func TestExtractValuesRejectsAnArchiveThatExpandsPastTheTotalBudget(t *testing.T) {
	// Zeroes compress to almost nothing, which is the whole trick: a few kilobytes
	// on the wire, tens of megabytes once walked.
	archive := buildOrderedChartArchive(t, []archiveEntry{
		{name: "podinfo/big.bin", content: strings.Repeat("\x00", 64<<20)},
		{name: "podinfo/values.yaml", content: "a: b\n"},
	})

	if limit := decompressedLimit(int64(len(archive))); limit >= 64<<20 {
		t.Fatalf("archive of %d bytes licenses %d decompressed bytes; the test payload no longer exceeds it", len(archive), limit)
	}

	_, err := extractValues(context.Background(), archive, testMaxBytes)
	if !errors.Is(err, ErrArtifactTooLarge) {
		t.Fatalf("expected ErrArtifactTooLarge, got %v", err)
	}
}

// TestExtractValuesRejectsAnArchiveWithTooManyEntries covers the other half of the
// same budget: entries that carry no content cost no bytes, so only counting them
// bounds an archive built out of headers alone.
func TestExtractValuesRejectsAnArchiveWithTooManyEntries(t *testing.T) {
	archive := buildArchiveWithEntries(t, maxArchiveEntries+1)

	_, err := extractValues(context.Background(), archive, testMaxBytes)
	if !errors.Is(err, ErrArtifactTooManyEntries) {
		t.Fatalf("expected ErrArtifactTooManyEntries, got %v", err)
	}
}

// TestExtractValuesAcceptsAnArchiveWithinBothBudgets is the other side of the two
// tests above: the bounds must not refuse a chart of the size real charts have.
func TestExtractValuesAcceptsAnArchiveWithinBothBudgets(t *testing.T) {
	archive := buildArchiveWithEntries(t, maxArchiveEntries-1)

	got, err := extractValues(context.Background(), archive, testMaxBytes)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(got) != "a: b\n" {
		t.Fatalf("values = %q", got)
	}
}

// TestExtractValuesStopsOnACancelledContext pins that the walk is abandonable: it
// used to run to the end of the archive whatever the caller's context said, so a
// request that had already gone away still paid for the whole extraction.
func TestExtractValuesStopsOnACancelledContext(t *testing.T) {
	archive := buildChartArchive(t, map[string]string{
		"podinfo/values.yaml": "a: b\n",
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := extractValues(ctx, archive, testMaxBytes)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

// TestBoundedReaderStopsACopyInFlight pins the half of cancellation a per-entry
// check cannot reach: one large entry is a single copy, and it has to be
// interruptible in the middle rather than only between entries.
func TestBoundedReaderStopsACopyInFlight(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	reader := &boundedReader{ctx: ctx, r: bytes.NewReader(make([]byte, 1<<20)), limit: 1 << 20}

	buf := make([]byte, 1024)
	if _, err := reader.Read(buf); err != nil {
		t.Fatalf("first read: %v", err)
	}

	cancel()

	if _, err := reader.Read(buf); !errors.Is(err, context.Canceled) {
		t.Fatalf("read after cancel = %v, want context.Canceled", err)
	}
}

func TestDecompressedLimit(t *testing.T) {
	cases := []struct {
		name       string
		compressed int64
		want       int64
	}{
		{"a tiny archive is judged by the floor, not by its ratio", 512, minDecompressedBytes},
		{"an ordinary archive is judged by the ratio", 4 << 20, 4 << 20 * maxDecompressionRatio},
		{"a huge archive is capped rather than multiplied", 1 << 40, maxDecompressedBytes},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := decompressedLimit(tc.compressed); got != tc.want {
				t.Fatalf("decompressedLimit(%d) = %d, want %d", tc.compressed, got, tc.want)
			}
		})
	}
}
