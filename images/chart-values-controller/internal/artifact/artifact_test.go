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
	"testing"
)

func buildChartArchive(t *testing.T, files map[string]string) []byte {
	t.Helper()

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	for name, content := range files {
		if err := tw.WriteHeader(&tar.Header{
			Name:     name,
			Mode:     0o644,
			Size:     int64(len(content)),
			Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatalf("writing tar header: %v", err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
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
