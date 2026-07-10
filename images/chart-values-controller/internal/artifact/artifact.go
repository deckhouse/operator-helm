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

// FetchValues downloads the chart artifact from url, verifies its integrity
// against digest (format "sha256:<hex>"), and returns the raw bytes of the
// chart's top-level values.yaml. Artifacts larger than maxBytes are rejected
// with ErrArtifactTooLarge.
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

	// Fast reject when the server advertises a size over the limit.
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

	return extractValues(data, maxBytes)
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

func extractValues(data []byte, maxBytes int64) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("opening gzip stream: %w", err)
	}
	defer func() { _ = gz.Close() }()

	tr := tar.NewReader(gz)

	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("reading tar stream: %w", err)
		}

		if header.Typeflag != tar.TypeReg {
			continue
		}

		// A packaged chart stores files under a single top-level directory,
		// e.g. "podinfo/values.yaml". Match values.yaml at exactly that depth.
		parts := strings.Split(path.Clean(header.Name), "/")
		if len(parts) == 2 && parts[1] == "values.yaml" {
			// Bound the decompressed read too, so a small archive cannot expand
			// into an oversized values.yaml (decompression bomb).
			content, err := io.ReadAll(io.LimitReader(tr, maxBytes+1))
			if err != nil {
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
