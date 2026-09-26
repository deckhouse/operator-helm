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
	"io"
	"net/http"
	"path"
	"strings"
)

// ErrValuesNotFound is returned when the chart archive contains no top-level
// values.yaml file.
var ErrValuesNotFound = errors.New("values.yaml not found in chart archive")

// ErrArtifactTooLarge is returned when a chart artifact exceeds the configured
// size limit. It guards against memory exhaustion from a malicious or broken
// source.
var ErrArtifactTooLarge = errors.New("chart artifact exceeds the configured size limit")

// ErrArtifactTooManyEntries is returned when a chart archive holds more files
// than any chart plausibly does. Entry headers cost time rather than memory, so
// the byte budget alone does not bound them.
var ErrArtifactTooManyEntries = errors.New("chart archive holds too many entries")

const (
	// maxArchiveEntries bounds how many tar entries are walked before the archive
	// is refused. A packaged chart — templates, CRDs and every vendored subchart —
	// stays in the hundreds, so this is roughly an order of magnitude of headroom
	// over the largest real chart and still refuses an archive built purely of
	// headers.
	maxArchiveEntries = 10_000

	// maxDecompressionRatio bounds the whole decompressed stream at a multiple of
	// the archive it came from. A chart is text, and text gzips well — a chart of
	// repetitive templates and CRDs reaches ratios around ten — so this leaves a
	// legitimate chart room while refusing the bomb, whose ratio runs into the
	// thousands.
	maxDecompressionRatio = 50

	// minDecompressedBytes is the floor under that bound: gzip's own framing makes
	// the ratio of a very small archive meaningless, and every real chart fits
	// here anyway. It has to clear what maxArchiveEntries entries cost in tar
	// headers alone (512 bytes each, about 5 MiB), or the byte bound would refuse
	// an archive of many small files before the entry bound ever judged it.
	minDecompressedBytes = 8 << 20

	// maxDecompressedBytes is the ceiling over it, so an archive near the
	// configured artifact size cannot license a multi-gigabyte read on the ratio
	// alone.
	maxDecompressedBytes = 512 << 20
)

// budget is what the whole extraction is allowed to cost. It is shared by every
// reader in the chain rather than held by one of them, because no single point in
// that chain sees all the bytes: tar fabricates the holes of a sparse entry out of
// nothing (archive/tar zeroReader), so counting only what leaves gzip misses them,
// while counting only what leaves tar misses the entries the walk skips over —
// skipping still decompresses them.
type budget struct {
	ctx   context.Context
	limit int64
	read  int64
}

// reader returns r bounded by this budget and by ctx. Every Read consults both, so
// a cancelled request stops mid-copy rather than at the next entry header.
func (b *budget) reader(r io.Reader) io.Reader {
	return &boundedReader{budget: b, r: r}
}

func (b *budget) tooLarge() error {
	return fmt.Errorf("%w (decompressed > %d bytes)", ErrArtifactTooLarge, b.limit)
}

type boundedReader struct {
	*budget
	r io.Reader
}

func (b *boundedReader) Read(p []byte) (int, error) {
	if err := b.ctx.Err(); err != nil {
		return 0, err
	}

	if b.read > b.limit {
		return 0, b.tooLarge()
	}

	// Read one byte past the limit, so reaching it exactly is still a complete
	// archive and only the next byte is a breach.
	if remaining := b.limit + 1 - b.read; remaining < int64(len(p)) {
		p = p[:remaining]
	}

	n, err := b.r.Read(p)
	b.read += int64(n)
	if b.read > b.limit {
		return n, b.tooLarge()
	}

	return n, err
}

// cancellableReader carries ctx into a reader that consumes many small pieces per
// Read of its own. gzip walks concatenated members inside a single Read
// (compress/gzip gunzip.go), returning no bytes until one of them holds data, so an
// archive of empty members would otherwise run to its end whatever ctx said.
type cancellableReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *cancellableReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}

	return c.r.Read(p)
}

// decompressedLimit is the total the archive of compressed bytes may expand to.
// The division keeps the multiplication from overflowing on an absurd limit.
func decompressedLimit(compressed int64) int64 {
	if compressed > maxDecompressedBytes/maxDecompressionRatio {
		return maxDecompressedBytes
	}

	return max(compressed*maxDecompressionRatio, minDecompressedBytes)
}

// FetchValues downloads the chart artifact from url, verifies its integrity
// against digest (format "sha256:<hex>"), and returns the raw bytes of the
// chart's top-level values.yaml. Artifacts larger than maxBytes are rejected
// with ErrArtifactTooLarge, as are archives that decompress past
// decompressedLimit or hold more than maxArchiveEntries entries. Extraction
// stops as soon as ctx is done.
func FetchValues(ctx context.Context, httpClient *http.Client, url, digest string, maxBytes int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("building artifact request: %w", err)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("downloading artifact: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("downloading artifact: unexpected status %s", resp.Status)
	}

	if resp.ContentLength > maxBytes {
		return nil, fmt.Errorf("%w (%d bytes > %d bytes)", ErrArtifactTooLarge, resp.ContentLength, maxBytes)
	}

	// Read one byte past the limit so an oversized body is detected rather than
	// silently truncated (which would otherwise surface as a digest mismatch).
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("reading artifact body: %w", err)
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("%w (> %d bytes)", ErrArtifactTooLarge, maxBytes)
	}

	if err := verifyDigest(data, digest); err != nil {
		return nil, err
	}

	return extractValues(ctx, data, maxBytes)
}

func verifyDigest(data []byte, digest string) error {
	algo, want, ok := strings.Cut(digest, ":")
	if !ok || algo != "sha256" {
		return fmt.Errorf("unsupported artifact digest %q", digest)
	}

	got := fmt.Sprintf("%x", sha256.Sum256(data))
	if got != want {
		return fmt.Errorf("artifact digest mismatch: want %s, got %s", want, got)
	}

	return nil
}

func extractValues(ctx context.Context, data []byte, maxBytes int64) ([]byte, error) {
	gz, err := gzip.NewReader(&cancellableReader{ctx: ctx, r: bytes.NewReader(data)})
	if err != nil {
		return nil, fmt.Errorf("opening gzip stream: %w", err)
	}
	defer func() { _ = gz.Close() }()

	spent := &budget{ctx: ctx, limit: decompressedLimit(int64(len(data)))}
	tr := tar.NewReader(spent.reader(gz))

	for entries := 0; ; entries++ {
		// The byte budget is only consulted while bytes move; a header-only entry
		// moves almost none, so cancellation is checked here as well.
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		if entries >= maxArchiveEntries {
			return nil, fmt.Errorf("%w (more than %d)", ErrArtifactTooManyEntries, maxArchiveEntries)
		}

		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			if errors.Is(err, ErrArtifactTooLarge) || errors.Is(err, context.Canceled) ||
				errors.Is(err, context.DeadlineExceeded) {
				// The reader's own verdict, reported as itself: wrapping it as a tar
				// failure would read as a broken chart rather than a refused one.
				return nil, err
			}

			return nil, fmt.Errorf("reading tar stream: %w", err)
		}

		if header.Typeflag != tar.TypeReg {
			continue
		}

		// A packaged chart stores files under a single top-level directory,
		// e.g. "podinfo/values.yaml". Match values.yaml at exactly that depth.
		parts := strings.Split(path.Clean(header.Name), "/")
		if len(parts) == 2 && parts[1] == "values.yaml" {
			// Read through the budget again rather than straight from tr: the holes
			// of a sparse entry never pass through gzip, so this is the only place
			// they are counted, and the only place a copy of one can be cancelled.
			// maxBytes bounds this one file on top of that, since what the caller can
			// be handed is smaller than what the archive may cost to walk.
			content, err := io.ReadAll(io.LimitReader(spent.reader(tr), maxBytes+1))
			if err != nil {
				if errors.Is(err, ErrArtifactTooLarge) || errors.Is(err, context.Canceled) ||
					errors.Is(err, context.DeadlineExceeded) {
					return nil, err
				}

				return nil, fmt.Errorf("reading values.yaml: %w", err)
			}
			if int64(len(content)) > maxBytes {
				return nil, fmt.Errorf("%w (values.yaml > %d bytes)", ErrArtifactTooLarge, maxBytes)
			}

			return content, nil
		}
	}

	return nil, ErrValuesNotFound
}
