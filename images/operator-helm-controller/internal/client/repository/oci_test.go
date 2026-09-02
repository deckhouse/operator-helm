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
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
	"github.com/google/go-containerregistry/pkg/v1/static"
	"github.com/google/go-containerregistry/pkg/v1/types"

	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
)

func TestFetchChartsOCIRejectsURLWithoutImageName(t *testing.T) {
	_, err := OCIRepositoryDefaultClient.FetchCharts(context.Background(), "oci://ghcr.io", nil, FetchOptions{})
	if err == nil {
		t.Fatal("expected error for url without an image name")
	}

	terminal, ok := AsTerminal(err)
	if !ok {
		t.Fatalf("expected terminal error, got %v", err)
	}
	if terminal.Reason != helmv1alpha1.ReasonInvalidRepositoryURL {
		t.Fatalf("expected reason %q, got %q", helmv1alpha1.ReasonInvalidRepositoryURL, terminal.Reason)
	}
}

func TestClassifyRemoteError(t *testing.T) {
	cases := []struct {
		name         string
		err          error
		wantTerminal bool
		wantReason   string
	}{
		{
			name:         "unauthorized is terminal",
			err:          &transport.Error{StatusCode: http.StatusUnauthorized},
			wantTerminal: true,
			wantReason:   helmv1alpha1.ReasonAuthenticationFailed,
		},
		{
			name:         "not found is terminal",
			err:          &transport.Error{StatusCode: http.StatusNotFound},
			wantTerminal: true,
			wantReason:   helmv1alpha1.ReasonSourceNotFound,
		},
		{
			name:         "server error is retriable",
			err:          &transport.Error{StatusCode: http.StatusBadGateway},
			wantTerminal: false,
		},
		{
			name:         "transport failure is retriable",
			err:          errors.New("dial tcp: connection refused"),
			wantTerminal: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyRemoteError(tc.err, "ghcr.io/example/chart")
			terminal, ok := AsTerminal(got)
			if ok != tc.wantTerminal {
				t.Fatalf("terminal=%v, want %v (err %v)", ok, tc.wantTerminal, got)
			}
			if tc.wantTerminal && terminal.Reason != tc.wantReason {
				t.Fatalf("expected reason %q, got %q", tc.wantReason, terminal.Reason)
			}
			if !errors.Is(got, tc.err) {
				t.Fatalf("classified error must wrap the original, got %v", got)
			}
		})
	}
}

const (
	helmConfigMediaType   = types.MediaType("application/vnd.cncf.helm.config.v1+json")
	currentChartMediaType = "application/vnd.cncf.helm.chart.content.v1.tar+gzip"
	legacyChartMediaType  = "application/tar+gzip"
)

// fakeRegistry is an in-memory OCI registry that can count manifest reads and force a
// status code for a specific tag. Counting is what proves the incremental behaviour:
// the number of manifest requests, not the returned versions, is the point of D3.
type fakeRegistry struct {
	inner  http.Handler
	mu     sync.Mutex
	gets   map[string]int
	forced map[string]int
}

func newFakeRegistry(t *testing.T) (*fakeRegistry, string) {
	t.Helper()

	f := &fakeRegistry{
		inner:  registry.New(registry.Logger(log.New(io.Discard, "", 0))),
		gets:   map[string]int{},
		forced: map[string]int{},
	}
	server := httptest.NewServer(f)
	t.Cleanup(server.Close)

	return f, strings.TrimPrefix(server.URL, "http://")
}

func (f *fakeRegistry) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if tag, ok := manifestTag(r); ok {
		f.mu.Lock()
		f.gets[tag]++
		status := f.forced[tag]
		f.mu.Unlock()

		if status != 0 {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"errors":[{"code":"UNKNOWN"}]}`))

			return
		}
	}

	f.inner.ServeHTTP(w, r)
}

// manifestTag reports the tag of a manifest read request. Digest references are
// ignored: only tag reads are counted and forced.
func manifestTag(r *http.Request) (string, bool) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return "", false
	}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) < 4 || parts[0] != "v2" || parts[len(parts)-2] != "manifests" {
		return "", false
	}
	ref := parts[len(parts)-1]
	if strings.Contains(ref, ":") {
		return "", false
	}

	return ref, true
}

func (f *fakeRegistry) force(tag string, status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.forced[tag] = status
}

func (f *fakeRegistry) manifestGets(tag string) int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.gets[tag]
}

// reset clears the recorded manifest request counts. remote.Write can itself read
// manifests while pushing test fixtures, so tests that assert on request counts must
// reset the counter after the setup pushes and before the measured call.
func (f *fakeRegistry) reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gets = map[string]int{}
}

// pushChart writes a single-layer artifact with the given config and layer media
// types under host/podinfo:tag.
func pushChart(t *testing.T, host, tag string, configType types.MediaType, layerTypes ...types.MediaType) {
	t.Helper()

	img := empty.Image
	for _, layerType := range layerTypes {
		appended, err := mutate.Append(img, mutate.Addendum{
			Layer:     static.NewLayer([]byte("chart-"+tag), layerType),
			MediaType: layerType,
		})
		if err != nil {
			t.Fatalf("appending layer: %v", err)
		}
		img = appended
	}

	img = mutate.MediaType(img, types.OCIManifestSchema1)
	img = mutate.ConfigMediaType(img, configType)

	ref, err := name.NewTag(host + "/podinfo:" + tag)
	if err != nil {
		t.Fatalf("parsing tag: %v", err)
	}
	if err := remote.Write(ref, img); err != nil {
		t.Fatalf("pushing %s: %v", ref, err)
	}
}

func fetchOne(t *testing.T, host string, opts FetchOptions) Chart {
	t.Helper()

	charts, err := OCIRepositoryDefaultClient.FetchCharts(context.Background(), "oci://"+host+"/podinfo", nil, opts)
	if err != nil {
		t.Fatalf("FetchCharts returned %v", err)
	}
	if len(charts) != 1 {
		t.Fatalf("expected one chart, got %d", len(charts))
	}

	return charts[0]
}

func versionByTag(t *testing.T, chart Chart, tag string) ChartVersion {
	t.Helper()

	for _, version := range chart.Versions {
		if version.Version.Original() == tag {
			return version
		}
	}
	t.Fatalf("version %q is missing from %v", tag, chart.Versions)

	return ChartVersion{}
}

func TestFetchChartsOCIResolvesLayerMediaTypes(t *testing.T) {
	_, host := newFakeRegistry(t)
	pushChart(t, host, "1.0.0", helmConfigMediaType, currentChartMediaType)
	pushChart(t, host, "1.0.1", helmConfigMediaType, legacyChartMediaType)
	pushChart(t, host, "1.0.2", "application/vnd.unknown.config.v1+json", legacyChartMediaType)

	chart := fetchOne(t, host, FetchOptions{})

	if got := versionByTag(t, chart, "1.0.0").MediaType; got != currentChartMediaType {
		t.Fatalf("1.0.0 media type is %q, want %q", got, currentChartMediaType)
	}
	if got := versionByTag(t, chart, "1.0.1").MediaType; got != legacyChartMediaType {
		t.Fatalf("1.0.1 media type is %q, want %q", got, legacyChartMediaType)
	}

	unsupported := versionByTag(t, chart, "1.0.2")
	if unsupported.MediaType != "" {
		t.Fatalf("1.0.2 must not carry a media type, got %q", unsupported.MediaType)
	}
	if unsupported.UnavailableReason != helmv1alpha1.UnavailableReasonUnsupportedMediaType {
		t.Fatalf("1.0.2 reason is %q, want %q", unsupported.UnavailableReason, helmv1alpha1.UnavailableReasonUnsupportedMediaType)
	}
	if !strings.Contains(unsupported.UnavailableMessage, "application/vnd.unknown.config.v1+json") {
		t.Fatalf("1.0.2 message %q must name the observed config type", unsupported.UnavailableMessage)
	}
}

func TestFetchChartsOCIRejectsIndexTag(t *testing.T) {
	_, host := newFakeRegistry(t)

	idx, err := random.Index(256, 1, 2)
	if err != nil {
		t.Fatalf("building index: %v", err)
	}
	ref, err := name.NewTag(host + "/podinfo:2.0.0")
	if err != nil {
		t.Fatalf("parsing tag: %v", err)
	}
	if err := remote.WriteIndex(ref, idx); err != nil {
		t.Fatalf("pushing index: %v", err)
	}

	version := versionByTag(t, fetchOne(t, host, FetchOptions{}), "2.0.0")
	if version.UnavailableReason != helmv1alpha1.UnavailableReasonUnsupportedMediaType {
		t.Fatalf("an index must be a verdict, got reason %q", version.UnavailableReason)
	}
}

func TestFetchChartsOCIMarksTransportFailurePending(t *testing.T) {
	reg, host := newFakeRegistry(t)
	pushChart(t, host, "1.0.0", helmConfigMediaType, currentChartMediaType)
	pushChart(t, host, "1.0.1", helmConfigMediaType, currentChartMediaType)
	reg.force("1.0.1", http.StatusInternalServerError)

	chart := fetchOne(t, host, FetchOptions{})

	if got := versionByTag(t, chart, "1.0.0").MediaType; got != currentChartMediaType {
		t.Fatalf("a healthy tag must still be published, got media type %q", got)
	}

	pending := versionByTag(t, chart, "1.0.1")
	if pending.UnavailableReason != helmv1alpha1.UnavailableReasonResolvePending {
		t.Fatalf("reason is %q, want %q", pending.UnavailableReason, helmv1alpha1.UnavailableReasonResolvePending)
	}
	if pending.UnavailableMessage == "" {
		t.Fatal("a pending version must carry the error message")
	}
}

func TestFetchChartsOCIDropsVanishedTag(t *testing.T) {
	reg, host := newFakeRegistry(t)
	pushChart(t, host, "1.0.0", helmConfigMediaType, currentChartMediaType)
	pushChart(t, host, "1.0.1", helmConfigMediaType, currentChartMediaType)
	reg.force("1.0.1", http.StatusNotFound)

	chart := fetchOne(t, host, FetchOptions{})

	for _, version := range chart.Versions {
		if version.Version.Original() == "1.0.1" {
			t.Fatal("a tag that 404s on its manifest must be omitted")
		}
	}
}

func TestFetchChartsOCIEscalatesUnauthorized(t *testing.T) {
	reg, host := newFakeRegistry(t)
	pushChart(t, host, "1.0.0", helmConfigMediaType, currentChartMediaType)
	reg.force("1.0.0", http.StatusUnauthorized)

	_, err := OCIRepositoryDefaultClient.FetchCharts(context.Background(), "oci://"+host+"/podinfo", nil, FetchOptions{})
	if err == nil {
		t.Fatal("expected an error")
	}
	terminal, ok := AsTerminal(err)
	if !ok {
		t.Fatalf("expected a terminal error, got %v", err)
	}
	if terminal.Reason != helmv1alpha1.ReasonAuthenticationFailed {
		t.Fatalf("reason is %q, want %q", terminal.Reason, helmv1alpha1.ReasonAuthenticationFailed)
	}
}

func TestFetchChartsOCISkipsKnownTags(t *testing.T) {
	reg, host := newFakeRegistry(t)
	pushChart(t, host, "1.0.0", helmConfigMediaType, currentChartMediaType)
	pushChart(t, host, "1.0.1", helmConfigMediaType, legacyChartMediaType)
	pushChart(t, host, "1.0.2", helmConfigMediaType, currentChartMediaType)
	pushChart(t, host, "1.0.3", helmConfigMediaType, currentChartMediaType)
	reg.reset()

	known := KnownCharts{"podinfo": KnownVersions{
		"1.0.0": {MediaType: currentChartMediaType},
		"1.0.1": {UnavailableReason: helmv1alpha1.UnavailableReasonUnsupportedMediaType},
		"1.0.2": {UnavailableReason: helmv1alpha1.UnavailableReasonResolvePending},
		// An entry written before media types were recorded: the upgrade migration.
		"1.0.3": {},
	}}

	chart := fetchOne(t, host, FetchOptions{Known: known})

	if got := reg.manifestGets("1.0.0"); got != 0 {
		t.Fatalf("a resolved tag must not be requested, got %d requests", got)
	}
	if got := reg.manifestGets("1.0.1"); got != 0 {
		t.Fatalf("an unsupported tag must not be requested, got %d requests", got)
	}
	if got := reg.manifestGets("1.0.2"); got != 1 {
		t.Fatalf("a pending tag must be requested once, got %d requests", got)
	}
	if got := reg.manifestGets("1.0.3"); got != 1 {
		t.Fatalf("an entry with no verdict must be requested once, got %d requests", got)
	}

	if got := versionByTag(t, chart, "1.0.0").MediaType; got != currentChartMediaType {
		t.Fatalf("a skipped tag must keep its media type, got %q", got)
	}
	if got := versionByTag(t, chart, "1.0.2").MediaType; got != currentChartMediaType {
		t.Fatalf("a re-examined tag must be resolved, got %q", got)
	}
	if got := versionByTag(t, chart, "1.0.3").MediaType; got != currentChartMediaType {
		t.Fatalf("a migrated tag must be resolved, got %q", got)
	}
}

func TestFetchChartsOCIFullIgnoresKnown(t *testing.T) {
	reg, host := newFakeRegistry(t)
	pushChart(t, host, "1.0.0", helmConfigMediaType, currentChartMediaType)
	reg.reset()

	known := KnownCharts{"podinfo": KnownVersions{"1.0.0": {MediaType: currentChartMediaType}}}
	fetchOne(t, host, FetchOptions{Known: known, Full: true})

	if got := reg.manifestGets("1.0.0"); got != 1 {
		t.Fatalf("a full pass must re-examine every tag, got %d requests", got)
	}
}

func TestFetchChartsOCIClearsRemovedFromRepository(t *testing.T) {
	_, host := newFakeRegistry(t)
	pushChart(t, host, "1.0.0", helmConfigMediaType, currentChartMediaType)

	known := KnownCharts{"podinfo": KnownVersions{"1.0.0": {
		MediaType:         currentChartMediaType,
		UnavailableReason: helmv1alpha1.UnavailableReasonRemovedFromRepository,
	}}}

	version := versionByTag(t, fetchOne(t, host, FetchOptions{Known: known}), "1.0.0")
	if version.UnavailableReason != "" {
		t.Fatalf("a listed tag must not stay removed, got reason %q", version.UnavailableReason)
	}
}
