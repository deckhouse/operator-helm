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

	spent := &budget{ctx: ctx, limit: 1 << 20}
	reader := spent.reader(bytes.NewReader(make([]byte, 1<<20)))

	buf := make([]byte, 1024)
	if _, err := reader.Read(buf); err != nil {
		t.Fatalf("first read: %v", err)
	}

	cancel()

	if _, err := reader.Read(buf); !errors.Is(err, context.Canceled) {
		t.Fatalf("read after cancel = %v, want context.Canceled", err)
	}
}

// TestCancellableReaderStopsAReaderThatYieldsNothing pins the half of cancellation
// the budget cannot reach. gzip walks concatenated members inside a single Read of
// its own, returning no bytes until one of them holds data, so a reader bounded
// only on its output would run an archive of empty members to the end whatever the
// request said. The check belongs on the compressed side, where every member is
// pulled from.
func TestCancellableReaderStopsAReaderThatYieldsNothing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	reader := &cancellableReader{ctx: ctx, r: bytes.NewReader(make([]byte, 1<<20))}

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

// TestExtractValuesRejectsASparseEntryThatFabricatesItsSize covers the one way an
// entry can cost more than it weighs. tar serves the holes of a sparse file out of
// its own zero reader without touching the stream underneath (archive/tar
// reader.go, zeroReader), so a budget counting only what leaves gzip never sees
// them: a three-kilobyte archive hands back megabytes having read two kilobytes.
//
// The size is chosen between the archive's budget and maxBytes, so that the
// per-file limit cannot be what refuses it — that limit already held, and it is
// the budget this test is about.
func TestExtractValuesRejectsASparseEntryThatFabricatesItsSize(t *testing.T) {
	archive := buildSparseArchive(t, "podinfo/values.yaml", 9<<20)

	if limit := decompressedLimit(int64(len(archive))); limit >= 9<<20 {
		t.Fatalf("archive of %d bytes licenses %d decompressed bytes; the entry no longer exceeds it",
			len(archive), limit)
	}
	if testMaxBytes <= 9<<20 {
		t.Fatal("the per-file limit would refuse this entry on its own; the test would prove nothing")
	}

	_, err := extractValues(context.Background(), archive, testMaxBytes)
	if !errors.Is(err, ErrArtifactTooLarge) {
		t.Fatalf("expected ErrArtifactTooLarge, got %v", err)
	}
}

// buildSparseArchive writes a PAX 1.0 sparse entry by hand: archive/tar writes no
// sparse files, so the bytes are assembled here. The entry declares realsize and
// carries a map saying all of it is a hole, which is what makes it free to produce.
func buildSparseArchive(t *testing.T, name string, realsize int64) []byte {
	t.Helper()

	records := paxRecord("GNU.sparse.major", "1") +
		paxRecord("GNU.sparse.minor", "0") +
		paxRecord("GNU.sparse.name", name) +
		paxRecord("GNU.sparse.realsize", fmt.Sprint(realsize))

	// One entry at offset zero holding zero bytes of data: the whole file is a hole.
	sparseMap := padToBlock([]byte("1\n0\n0\n"))

	var tarball bytes.Buffer
	tarball.Write(tarHeader(t, "PaxHeaders/0", int64(len(records)), tar.TypeXHeader))
	tarball.Write(padToBlock([]byte(records)))
	tarball.Write(tarHeader(t, "GNUSparseFile.0/"+name, int64(len(sparseMap)), tar.TypeReg))
	tarball.Write(sparseMap)
	tarball.Write(make([]byte, 1024)) // the two empty blocks that end an archive

	var out bytes.Buffer
	gz := gzip.NewWriter(&out)
	if _, err := gz.Write(tarball.Bytes()); err != nil {
		t.Fatalf("compressing archive: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("closing gzip writer: %v", err)
	}

	return out.Bytes()
}

// tarHeader writes one ustar header block, checksum included.
func tarHeader(t *testing.T, name string, size int64, typeflag byte) []byte {
	t.Helper()

	block := make([]byte, 512)
	copy(block[0:100], name)
	copy(block[100:108], "0000644\x00")
	copy(block[124:136], fmt.Sprintf("%011o\x00", size))
	copy(block[136:148], fmt.Sprintf("%011o\x00", 0))
	block[156] = typeflag
	copy(block[257:263], "ustar\x00")
	copy(block[263:265], "00")

	// The checksum is computed with its own field read as spaces.
	for i := 148; i < 156; i++ {
		block[i] = ' '
	}
	sum := 0
	for _, b := range block {
		sum += int(b)
	}
	copy(block[148:156], fmt.Sprintf("%06o\x00 ", sum))

	return block
}

// paxRecord formats one "<length> <key>=<value>\n" record, whose length counts
// itself and so has to be solved for.
func paxRecord(key, value string) string {
	for n := len(key) + len(value) + 5; ; n++ {
		record := fmt.Sprintf("%d %s=%s\n", n, key, value)
		if len(record) == n {
			return record
		}
	}
}

func padToBlock(b []byte) []byte {
	for len(b)%512 != 0 {
		b = append(b, 0)
	}

	return b
}
