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

// ErrArtifactTooLarge is returned when a chart artifact exceeds the configured size limit.
var ErrArtifactTooLarge = errors.New("chart artifact exceeds the configured size limit")

// ErrArtifactTooManyEntries is returned when a chart archive holds more files than any
// chart plausibly does, which the byte budget alone does not bound.
var ErrArtifactTooManyEntries = errors.New("chart archive holds too many entries")

const (
	// maxArchiveEntries bounds how many tar entries are walked before the archive is refused, about an order of magnitude over the largest real chart.
	maxArchiveEntries = 10_000

	// maxDecompressionRatio bounds the decompressed stream at a multiple of the archive it came from; a real chart of templates and CRDs reaches ratios around ten.
	maxDecompressionRatio = 50

	// minDecompressedBytes is the floor under that bound; it clears what maxArchiveEntries tar headers cost on their own (512 bytes each, about 5 MiB).
	minDecompressedBytes = 8 << 20

	// maxDecompressedBytes is the ceiling over it, so a large archive cannot license a multi-gigabyte read on the ratio alone.
	maxDecompressedBytes = 512 << 20
)

// budget is what the whole extraction is allowed to cost, counted at what leaves gzip, which every byte the archive carries passes — skipped entries included.
type budget struct {
	ctx   context.Context
	limit int64
	read  int64
}

// reader returns r bounded by this budget and by ctx, and is what keeps the counter.
func (b *budget) reader(r io.Reader) io.Reader {
	return &boundedReader{budget: b, r: r}
}

// afford reports whether an entry that declares size fits in what is left, booking nothing. It catches the sparse entry, whose holes tar fabricates out of nothing (archive/tar zeroReader) and so never pass a reader.
func (b *budget) afford(size int64) error {
	if size > b.limit-b.read {
		return b.tooLarge()
	}

	return nil
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

	// Read one byte past the limit, so reaching it exactly is still a complete archive.
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

// cancellableReader carries ctx into a reader that consumes many small pieces per Read of its own, as gzip does walking concatenated members (compress/gzip gunzip.go).
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

// decompressedLimit is the total an archive of compressed bytes may expand to; the division keeps the multiplication from overflowing.
func decompressedLimit(compressed int64) int64 {
	if compressed > maxDecompressedBytes/maxDecompressionRatio {
		return maxDecompressedBytes
	}

	return max(compressed*maxDecompressionRatio, minDecompressedBytes)
}

// FetchValues downloads the chart artifact from url, verifies it against digest (format
// "sha256:<hex>") and returns the raw bytes of the chart's top-level values.yaml.
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

	// Read one byte past the limit so an oversized body is detected rather than silently truncated.
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
		// The byte budget is only consulted while bytes move, and a header-only entry moves almost none.
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
				// The reader's own verdict, reported as itself rather than as a tar failure.
				return nil, err
			}

			return nil, fmt.Errorf("reading tar stream: %w", err)
		}

		if header.Typeflag != tar.TypeReg {
			continue
		}

		// A packaged chart stores files under a single top-level directory, e.g. "podinfo/values.yaml".
		parts := strings.Split(path.Clean(header.Name), "/")
		if len(parts) == 2 && parts[1] == "values.yaml" {
			if err := spent.afford(header.Size); err != nil {
				return nil, err
			}

			// maxBytes bounds this one file on top of the budget, being smaller than what the archive may cost to walk.
			content, err := io.ReadAll(io.LimitReader(&cancellableReader{ctx: ctx, r: tr}, maxBytes+1))
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
