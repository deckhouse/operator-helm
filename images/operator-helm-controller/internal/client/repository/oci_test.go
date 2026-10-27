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
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

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

type fakeRegistry struct {
	inner       http.Handler
	mu          sync.Mutex
	gets        map[string]int
	forced      map[string]int
	stalledTags map[string]bool
	stall       chan struct{}
}

func newFakeRegistry(t *testing.T) (*fakeRegistry, string) {
	t.Helper()

	f := &fakeRegistry{
		inner:       registry.New(registry.Logger(log.New(io.Discard, "", 0))),
		gets:        map[string]int{},
		forced:      map[string]int{},
		stalledTags: map[string]bool{},
		stall:       make(chan struct{}),
	}
	server := httptest.NewServer(f)
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(f.stall) })

	return f, strings.TrimPrefix(server.URL, "http://")
}

func (f *fakeRegistry) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if tag, ok := manifestTag(r); ok {
		f.mu.Lock()
		f.gets[tag]++
		status := f.forced[tag]
		stalling := f.stalledTags[tag]
		f.mu.Unlock()

		if stalling {
			select {
			case <-f.stall:
			case <-r.Context().Done():
			}

			return
		}

		if status != 0 {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"errors":[{"code":"UNKNOWN"}]}`))

			return
		}
	}

	f.inner.ServeHTTP(w, r)
}

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

func (f *fakeRegistry) stallTag(tag string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stalledTags[tag] = true
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

func (f *fakeRegistry) reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gets = map[string]int{}
}

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

	if got := versionByTag(t, chart, "1.0.0").MediaType; got != currentChartMediaType {
		t.Fatalf("a healthy tag must still be published, got media type %q", got)
	}

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

func TestFetchChartsOCIResolveVersionsSurfacesCancelledContext(t *testing.T) {
	_, host := newFakeRegistry(t)
	pushChart(t, host, "1.0.0", helmConfigMediaType, currentChartMediaType)

	repo, err := name.NewRepository(host + "/podinfo")
	if err != nil {
		t.Fatalf("parsing repository: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	versions, unexamined, err := resolveChartVersions(
		ctx, repo, "podinfo", []string{"1.0.0"}, []remote.Option{remote.WithContext(ctx)}, FetchOptions{},
	)
	if err == nil {
		t.Fatal("a cancelled context must not produce a completed pass")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected an error wrapping context.Canceled, got %v", err)
	}
	if versions != nil || unexamined != nil {
		t.Fatalf("an abandoned pass must publish nothing, got versions %+v and unexamined %v", versions, unexamined)
	}
}

func TestFetchChartsOCISkipsKnownTags(t *testing.T) {
	reg, host := newFakeRegistry(t)
	pushChart(t, host, "1.0.0", helmConfigMediaType, currentChartMediaType)
	pushChart(t, host, "1.0.1", helmConfigMediaType, legacyChartMediaType)
	pushChart(t, host, "1.0.2", helmConfigMediaType, currentChartMediaType)
	pushChart(t, host, "1.0.3", helmConfigMediaType, currentChartMediaType)
	reg.reset()

	const unsupportedMessage = "config media type recorded by a previous pass is not a helm chart config"

	known := KnownCharts{"podinfo": KnownVersions{
		"1.0.0": {MediaType: currentChartMediaType},
		"1.0.1": {
			UnavailableReason:  helmv1alpha1.UnavailableReasonUnsupportedMediaType,
			UnavailableMessage: unsupportedMessage,
		},
		"1.0.2": {UnavailableReason: helmv1alpha1.UnavailableReasonResolvePending},
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
	if got := versionByTag(t, chart, "1.0.1").UnavailableReason; got != helmv1alpha1.UnavailableReasonUnsupportedMediaType {
		t.Fatalf("a skipped unsupported tag must keep its recorded reason, got %q, want %q",
			got, helmv1alpha1.UnavailableReasonUnsupportedMediaType)
	}
	if got := versionByTag(t, chart, "1.0.1").UnavailableMessage; got != unsupportedMessage {
		t.Fatalf("a skipped unsupported tag must keep its recorded message, got %q, want %q", got, unsupportedMessage)
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

func TestFetchChartsOCIReexaminesATagRecordedAsRemoved(t *testing.T) {
	reg, host := newFakeRegistry(t)
	pushChart(t, host, "1.0.0", helmConfigMediaType, currentChartMediaType)
	reg.reset()

	known := KnownCharts{"podinfo": KnownVersions{"1.0.0": {
		MediaType:          legacyChartMediaType,
		UnavailableReason:  helmv1alpha1.UnavailableReasonRemovedFromRepository,
		UnavailableMessage: "no longer offered by the repository",
	}}}

	version := versionByTag(t, fetchOne(t, host, FetchOptions{Known: known}), "1.0.0")

	if got := reg.manifestGets("1.0.0"); got != 1 {
		t.Fatalf("a tag the registry offers again must be examined once, got %d requests", got)
	}
	if version.UnavailableReason != "" {
		t.Fatalf("a listed tag must not stay removed, got reason %q", version.UnavailableReason)
	}
	if version.UnavailableMessage != "" {
		t.Fatalf("clearing the removed marker must also clear its message, got %q", version.UnavailableMessage)
	}
	if version.MediaType != currentChartMediaType {
		t.Fatalf("media type = %q, want the one the re-pushed artifact holds (%q)", version.MediaType, currentChartMediaType)
	}
}

func TestTruncateCutsOnARuneBoundary(t *testing.T) {
	message := strings.Repeat("☃", unavailableMessageLimit/3+2)

	got := truncate(message)

	if !utf8.ValidString(got) {
		t.Fatalf("truncate produced invalid UTF-8: %q", got)
	}
	if len(got) > unavailableMessageLimit+len("…") {
		t.Fatalf("truncated message is %d bytes, want at most %d", len(got), unavailableMessageLimit+len("…"))
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("a truncated message must end with the ellipsis marker, got %q", got)
	}
}

func TestTruncateLeavesAShortMessageUnchanged(t *testing.T) {
	message := "config media type is not a helm chart config"

	if got := truncate(message); got != message {
		t.Fatalf("truncate(%q) = %q, want it unchanged", message, got)
	}
}

func shortenOCILimits(t *testing.T, list, probe, pass time.Duration) {
	t.Helper()

	listBefore, probeBefore, passBefore := tagListBudget, chartProbeTimeout, chartResolvePassBudget

	tagListBudget, chartProbeTimeout, chartResolvePassBudget = list, probe, pass

	t.Cleanup(func() {
		tagListBudget, chartProbeTimeout, chartResolvePassBudget = listBefore, probeBefore, passBefore
	})
}

func hangingRegistryHost(t *testing.T) string {
	t.Helper()

	hang := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-hang:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() {
		close(hang)
		server.Close()
	})

	return strings.TrimPrefix(server.URL, "http://")
}

func TestFetchChartsOCIAbandonsARegistryThatNeverAnswersTheListing(t *testing.T) {
	shortenOCILimits(t, 300*time.Millisecond, chartProbeTimeout, chartResolvePassBudget)

	host := hangingRegistryHost(t)

	start := time.Now()
	charts, err := OCIRepositoryDefaultClient.FetchCharts(
		context.Background(), "oci://"+host+"/podinfo", nil, FetchOptions{},
	)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("a registry that never answers must fail the listing, got %d charts", len(charts))
	}
	if elapsed > 10*time.Second {
		t.Fatalf("the listing took %v: the budget did not bound it", elapsed)
	}
	if _, ok := AsTerminal(err); ok {
		t.Fatalf("error %v must stay retriable: a stalled registry may answer next time", err)
	}
	if !strings.Contains(err.Error(), "listing image tags") {
		t.Fatalf("error %v must say which call was abandoned", err)
	}
}

func TestFetchChartsOCIPublishesWhatAnExpiredPassExamined(t *testing.T) {
	shortenOCILimits(t, tagListBudget, 10*time.Second, 500*time.Millisecond)

	reg, host := newFakeRegistry(t)
	pushChart(t, host, "1.0.0", helmConfigMediaType, currentChartMediaType)
	pushChart(t, host, "1.0.1", helmConfigMediaType, currentChartMediaType)
	pushChart(t, host, "1.0.2", helmConfigMediaType, currentChartMediaType)
	reg.stallTag("1.0.1")

	known := KnownCharts{"podinfo": KnownVersions{"1.0.2": {MediaType: legacyChartMediaType}}}

	start := time.Now()
	charts, err := OCIRepositoryDefaultClient.FetchCharts(
		context.Background(), "oci://"+host+"/podinfo", nil, FetchOptions{Known: known},
	)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("an expired budget must not fail the pass, got %v", err)
	}
	if elapsed > 10*time.Second {
		t.Fatalf("the pass took %v: the budget did not bound it", elapsed)
	}
	if len(charts) != 1 {
		t.Fatalf("expected one chart, got %d", len(charts))
	}

	if got := versionByTag(t, charts[0], "1.0.0").MediaType; got != currentChartMediaType {
		t.Fatalf("a tag examined before the budget expired must be published, got media type %q", got)
	}
	if got := versionByTag(t, charts[0], "1.0.2").MediaType; got != legacyChartMediaType {
		t.Fatalf("a carried verdict must survive an expired budget, got media type %q", got)
	}

	for _, version := range charts[0].Versions {
		if version.Version.Original() == "1.0.1" {
			t.Fatalf("the unexamined tag must carry no verdict, got %+v", version)
		}
	}
	if got := charts[0].UnexaminedVersions; len(got) != 1 || got[0] != "1.0.1" {
		t.Fatalf("unexamined versions = %v, want exactly the stalled tag", got)
	}
	if reg.manifestGets("1.0.1") == 0 {
		t.Fatal("the stalled tag must have been attempted at all")
	}
}

func TestFetchChartsOCIAbandonsThePassWhenTheParentDeadlineIsGone(t *testing.T) {
	shortenOCILimits(t, tagListBudget, 10*time.Second, 30*time.Second)

	reg, host := newFakeRegistry(t)
	pushChart(t, host, "1.0.0", helmConfigMediaType, currentChartMediaType)
	pushChart(t, host, "1.0.1", helmConfigMediaType, currentChartMediaType)
	reg.stallTag("1.0.1")

	repo, err := name.NewRepository(host + "/podinfo")
	if err != nil {
		t.Fatalf("parsing repository: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	versions, unexamined, err := resolveChartVersions(
		ctx, repo, "podinfo", []string{"1.0.0", "1.0.1"}, []remote.Option{remote.WithContext(ctx)}, FetchOptions{},
	)
	if err == nil {
		t.Fatalf("a dead parent context must fail the pass, got versions %+v and unexamined %v", versions, unexamined)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error %v must report the expired deadline", err)
	}
	if _, ok := AsTerminal(err); ok {
		t.Fatalf("error %v must stay retriable: the next reconcile is worth running", err)
	}
}

func TestFetchChartsOCIRecordsASingleStalledTagAsPending(t *testing.T) {
	shortenOCILimits(t, tagListBudget, 300*time.Millisecond, 30*time.Second)

	reg, host := newFakeRegistry(t)
	pushChart(t, host, "1.0.0", helmConfigMediaType, currentChartMediaType)
	pushChart(t, host, "1.0.1", helmConfigMediaType, currentChartMediaType)
	reg.stallTag("1.0.1")

	start := time.Now()
	charts, err := OCIRepositoryDefaultClient.FetchCharts(
		context.Background(), "oci://"+host+"/podinfo", nil, FetchOptions{},
	)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("a stalled tag is a verdict, not a failed pass, got %v", err)
	}
	if elapsed > 10*time.Second {
		t.Fatalf("the probes took %v: the per-probe timeout did not bound them", elapsed)
	}
	if len(charts) != 1 {
		t.Fatalf("expected one chart, got %d", len(charts))
	}

	reasons := map[string]string{}
	for _, version := range charts[0].Versions {
		reasons[version.Version.String()] = version.UnavailableReason
	}
	if got := reasons["1.0.0"]; got != "" {
		t.Fatalf("the tag that answered must be usable, got reason %q", got)
	}
	if got := reasons["1.0.1"]; got != helmv1alpha1.UnavailableReasonResolvePending {
		t.Fatalf("the stalled tag reason = %q, want %q", got, helmv1alpha1.UnavailableReasonResolvePending)
	}
}

func TestFetchChartsOCIExaminesANeverSeenTagAheadOfAStalledOne(t *testing.T) {
	shortenOCILimits(t, tagListBudget, 200*time.Millisecond, time.Second)

	reg, host := newFakeRegistry(t)

	const (
		stallingTags = 80
		healthyTag   = "9.0.0"
	)

	stalled := make([]string, 0, stallingTags)

	for i := range stallingTags {
		tag := fmt.Sprintf("0.0.%d", i+1)
		pushChart(t, host, tag, helmConfigMediaType, currentChartMediaType)
		stalled = append(stalled, tag)
	}

	pushChart(t, host, healthyTag, helmConfigMediaType, currentChartMediaType)

	for _, tag := range stalled {
		reg.stallTag(tag)
	}

	first := fetchOne(t, host, FetchOptions{})
	if !slices.Contains(first.UnexaminedVersions, healthyTag) {
		t.Fatalf("the first pass was expected to run out of budget before %s; unexamined = %v",
			healthyTag, first.UnexaminedVersions)
	}

	known := make(KnownVersions, len(stalled))
	for _, tag := range stalled {
		known[tag] = KnownVersion{UnavailableReason: helmv1alpha1.UnavailableReasonResolvePending}
	}

	reg.reset()

	second := fetchOne(t, host, FetchOptions{Known: KnownCharts{"podinfo": known}})

	if got := versionByTag(t, second, healthyTag).MediaType; got != currentChartMediaType {
		t.Fatalf("%s media type is %q, want %q: a never seen tag must be examined before the stalled ones",
			healthyTag, got, currentChartMediaType)
	}

	probed := 0
	for _, tag := range stalled {
		probed += reg.manifestGets(tag)
	}
	if probed == 0 {
		t.Fatal("the pending tags must still be re-examined with the budget left after the new ones")
	}
}

func TestFetchChartsOCIExaminesAMarkedTagAheadOfAStalledOne(t *testing.T) {
	shortenOCILimits(t, tagListBudget, 200*time.Millisecond, time.Second)

	reg, host := newFakeRegistry(t)

	const (
		stallingTags = 80
		markedTag    = "9.0.0"
		relistedTag  = "9.0.1"
	)

	known := make(KnownVersions, stallingTags+2)

	for i := range stallingTags {
		tag := fmt.Sprintf("0.0.%d", i+1)
		pushChart(t, host, tag, helmConfigMediaType, currentChartMediaType)
		known[tag] = KnownVersion{UnavailableReason: helmv1alpha1.UnavailableReasonResolvePending}
	}

	pushChart(t, host, markedTag, helmConfigMediaType, currentChartMediaType)
	known[markedTag] = KnownVersion{MediaType: legacyChartMediaType, ReexaminationRequired: true}

	pushChart(t, host, relistedTag, helmConfigMediaType, currentChartMediaType)
	known[relistedTag] = KnownVersion{
		MediaType:          legacyChartMediaType,
		UnavailableReason:  helmv1alpha1.UnavailableReasonRemovedFromRepository,
		UnavailableMessage: "the repository no longer offers this version",
	}

	for i := range stallingTags {
		reg.stallTag(fmt.Sprintf("0.0.%d", i+1))
	}

	for pass := 1; pass <= 2; pass++ {
		reg.reset()

		chart := fetchOne(t, host, FetchOptions{Known: KnownCharts{"podinfo": known}})

		if got := versionByTag(t, chart, markedTag).MediaType; got != currentChartMediaType {
			t.Fatalf("pass %d: %s media type is %q, want %q: a marked tag must be examined before the stalled ones",
				pass, markedTag, got, currentChartMediaType)
		}

		relisted := versionByTag(t, chart, relistedTag)
		if relisted.UnavailableReason != "" {
			t.Fatalf("pass %d: %s reason is %q, want none: a tag the listing offers again must not stay removed",
				pass, relistedTag, relisted.UnavailableReason)
		}
		if relisted.MediaType != currentChartMediaType {
			t.Fatalf("pass %d: %s media type is %q, want %q: a relisted tag must be examined ahead of the stalling retries",
				pass, relistedTag, relisted.MediaType, currentChartMediaType)
		}

		probed := 0
		for i := range stallingTags {
			probed += reg.manifestGets(fmt.Sprintf("0.0.%d", i+1))
		}
		if probed == 0 {
			t.Fatalf("pass %d: the pending tags must still be re-examined with the budget left after the marked one", pass)
		}

		known = nextKnown(known, chart)
	}
}

func TestFetchChartsOCIOrdersAForcedPassByWhatIsRecorded(t *testing.T) {
	shortenOCILimits(t, tagListBudget, 200*time.Millisecond, time.Second)

	reg, host := newFakeRegistry(t)

	const (
		stallingTags = 80
		newTag       = "9.0.0"
	)

	known := make(KnownVersions, stallingTags)

	for i := range stallingTags {
		tag := fmt.Sprintf("0.0.%d", i+1)
		pushChart(t, host, tag, helmConfigMediaType, currentChartMediaType)
		known[tag] = KnownVersion{MediaType: currentChartMediaType}
	}

	pushChart(t, host, newTag, helmConfigMediaType, currentChartMediaType)

	for i := range stallingTags {
		reg.stallTag(fmt.Sprintf("0.0.%d", i+1))
	}
	reg.reset()

	chart := fetchOne(t, host, FetchOptions{Known: KnownCharts{"podinfo": known}, Full: true})

	if got := versionByTag(t, chart, newTag).MediaType; got != currentChartMediaType {
		t.Fatalf("%s media type is %q, want %q: a forced pass must order by what is recorded, not by what it examines",
			newTag, got, currentChartMediaType)
	}
}

func markAll(known KnownVersions) KnownVersions {
	marked := make(KnownVersions, len(known))
	for tag, version := range known {
		version.ReexaminationRequired = true
		marked[tag] = version
	}

	return marked
}

func nextKnown(previous KnownVersions, chart Chart) KnownVersions {
	next := make(KnownVersions, len(previous))

	for _, version := range chart.Versions {
		next[version.Version.Original()] = KnownVersion{
			MediaType:          version.MediaType,
			UnavailableReason:  version.UnavailableReason,
			UnavailableMessage: version.UnavailableMessage,
		}
	}

	for _, tag := range chart.UnexaminedVersions {
		if recorded, ok := previous[tag]; ok {
			next[tag] = recorded
		}
	}

	return next
}

func TestFetchChartsOCIReexaminesARepushedTagAForcedPassCouldNotReach(t *testing.T) {
	shortenOCILimits(t, tagListBudget, 200*time.Millisecond, 400*time.Millisecond)

	reg, host := newFakeRegistry(t)

	const (
		blockingTags = 40
		repushedTag  = "9.0.0"
	)

	known := make(KnownVersions, blockingTags+1)

	for i := range blockingTags {
		tag := fmt.Sprintf("0.0.%d", i+1)
		pushChart(t, host, tag, helmConfigMediaType, currentChartMediaType)
		known[tag] = KnownVersion{MediaType: currentChartMediaType}
	}

	pushChart(t, host, repushedTag, helmConfigMediaType, currentChartMediaType)
	known[repushedTag] = KnownVersion{MediaType: legacyChartMediaType}

	for i := range blockingTags {
		reg.stallTag(fmt.Sprintf("0.0.%d", i+1))
	}
	reg.reset()

	known = markAll(known)

	forced := fetchOne(t, host, FetchOptions{Known: KnownCharts{"podinfo": known}, Full: true})

	if !slices.Contains(forced.UnexaminedVersions, repushedTag) {
		t.Fatalf("the forced pass was expected to run out of budget before %s; unexamined = %v",
			repushedTag, forced.UnexaminedVersions)
	}

	known = nextKnown(known, forced)

	shortenOCILimits(t, tagListBudget, 200*time.Millisecond, 10*time.Second)
	reg.reset()

	second := fetchOne(t, host, FetchOptions{Known: KnownCharts{"podinfo": known}})

	if got := reg.manifestGets(repushedTag); got == 0 {
		t.Fatal("the re-pushed tag must be examined by the pass that follows the forced one")
	}
	if got := versionByTag(t, second, repushedTag).MediaType; got != currentChartMediaType {
		t.Fatalf("%s media type is %q, want %q: the refresh must reach the tag it was asked for",
			repushedTag, got, currentChartMediaType)
	}
	if nextKnown(known, second)[repushedTag].ReexaminationRequired {
		t.Fatal("the pass that examined the re-pushed tag must clear the mark, or the refresh never ends")
	}
}

func TestFetchChartsOCIOrdersVersionsNewestFirst(t *testing.T) {
	_, host := newFakeRegistry(t)
	pushChart(t, host, "1.0.0", helmConfigMediaType, currentChartMediaType)
	pushChart(t, host, "2.0.0", helmConfigMediaType, currentChartMediaType)
	pushChart(t, host, "1.10.0", helmConfigMediaType, currentChartMediaType)

	chart := fetchOne(t, host, FetchOptions{})

	got := make([]string, 0, len(chart.Versions))
	for _, version := range chart.Versions {
		got = append(got, version.Version.Original())
	}

	want := []string{"2.0.0", "1.10.0", "1.0.0"}
	if !slices.Equal(got, want) {
		t.Fatalf("versions = %v, want %v: the catalog reads the newest version off the front", got, want)
	}
}
