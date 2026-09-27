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

// fakeRegistry is an in-memory OCI registry that can count manifest reads and force a
// status code for a specific tag. Counting is what proves the incremental behaviour:
// the number of manifest requests, not the returned versions, is the point of D3.
type fakeRegistry struct {
	inner  http.Handler
	mu     sync.Mutex
	gets   map[string]int
	forced map[string]int
	// stalledTags makes the manifest reads of the named tags hang until stall is
	// closed, which models the registry that accepts the request and then says
	// nothing. It is per tag so that one pass can be observed with both kinds of
	// answer: the tags that are left stalling are exactly the ones a pass has to
	// report as unexamined rather than guess a verdict for.
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
	// Registered last and therefore run first: a stalled handler outlives the request
	// that started it, and httptest waits for its handlers before closing.
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

// stallTag makes manifest reads of one tag hang while every other tag keeps being
// served, which is what a pass that must publish some versions and defer one looks like.
// Tag listing keeps working, so a pass reaches the per-tag probes either way.
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

// reset clears the recorded manifest request counts. remote.Write can itself read
// manifests while pushing test fixtures, so tests that assert on request counts must
// reset the counter after the setup pushes and before the measured call.
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

// TestFetchChartsOCIResolveVersionsSurfacesCancelledContext exercises resolveChartVersions
// directly rather than through FetchCharts: remote.List already fails a cancelled context
// on its own (unaffected by this fix), so reproducing the bug end-to-end would require
// cancelling the context after the listing but during the per-tag resolve, which is timing
// dependent. Calling the resolve stage directly reproduces it deterministically: without
// the ctx.Err() check, every goroutine's remote.Get fails with a wrapped context.Canceled
// that is not a *transport.Error, so it falls through to a fabricated ResolvePending verdict
// and resolveChartVersions returns a clean result with a nil error.
//
// A cancelled parent is the one context error the pass does not survive: an expired pass
// budget publishes what it examined, but a parent that is gone means the controller is
// shutting down or the reconcile deadline has passed, and there is nobody left to write
// the catalog this pass would hand over.
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

	// The message a previous pass recorded for the unsupported verdict. A skipped tag
	// is carried through verbatim, so this is test data rather than a message that
	// examineManifest would actually produce for this fixture.
	const unsupportedMessage = "config media type recorded by a previous pass is not a helm chart config"

	known := KnownCharts{"podinfo": KnownVersions{
		"1.0.0": {MediaType: currentChartMediaType},
		"1.0.1": {
			UnavailableReason:  helmv1alpha1.UnavailableReasonUnsupportedMediaType,
			UnavailableMessage: unsupportedMessage,
		},
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

// TestFetchChartsOCIReexaminesATagRecordedAsRemoved covers the one way a tag comes
// back. A version recorded as removed is kept only because a consumer references it,
// and the media type next to that reason is the deleted artifact's. A listing that
// offers the tag again therefore describes content that was pushed after the verdict
// was reached, and carrying the recorded verdict forward would republish the media type
// of an artifact the registry no longer holds — with nothing left to correct it, since
// the entry no longer looks removed to any later pass either.
func TestFetchChartsOCIReexaminesATagRecordedAsRemoved(t *testing.T) {
	reg, host := newFakeRegistry(t)
	pushChart(t, host, "1.0.0", helmConfigMediaType, currentChartMediaType)
	reg.reset()

	// The recorded media type is the one the deleted artifact had, so a carried verdict
	// is distinguishable from a fresh one.
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
	// Every rune here is a 3-byte UTF-8 sequence (☃, U+2603), chosen so that the byte
	// limit lands in the middle of one of them: a naive byte slice would corrupt it.
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

// shortenOCILimits replaces the registry call bounds for one test, so the behaviour
// they guard can be exercised without waiting out real minutes.
func shortenOCILimits(t *testing.T, list, probe, pass time.Duration) {
	t.Helper()

	listBefore, probeBefore, passBefore := tagListBudget, chartProbeTimeout, chartResolvePassBudget

	tagListBudget, chartProbeTimeout, chartResolvePassBudget = list, probe, pass

	t.Cleanup(func() {
		tagListBudget, chartProbeTimeout, chartResolvePassBudget = listBefore, probeBefore, passBefore
	})
}

// hangingRegistryHost starts a server that accepts every request and never answers
// it, which is the failure no transport-level timeout of go-containerregistry catches:
// the dial and the TLS handshake both succeed. It returns the host to address it by.
func hangingRegistryHost(t *testing.T) string {
	t.Helper()

	// The handlers outlive their requests on purpose and are released before the
	// server is closed, since httptest waits for its handlers.
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

// TestFetchChartsOCIAbandonsARegistryThatNeverAnswersTheListing pins that the tag
// listing is bounded. Without a budget the call inherits the reconcile context, which
// has no deadline, and go-containerregistry's transport bounds the dial and the
// handshake but not the wait for a response: one such repository holds a worker of
// the repository controller — which runs two — until the process restarts.
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

// TestFetchChartsOCIPublishesWhatAnExpiredPassExamined pins what the pass budget is
// for: it bounds how much one pass does, not whether the pass counts. A repository
// with more unexamined tags than the budget allows would otherwise never converge —
// every pass would throw away what it had resolved, so every next pass would start
// from the same empty known state and die at the same point, forever.
//
// The three tags stand for the three outcomes such a pass has: one already carries a
// recorded verdict, one is examined in time, and one stalls until the budget is gone.
// The stalled one is the delicate case — its probe fails on the pass's own context,
// which resolveChartVersion cannot tell from a registry failure and turns into a
// ResolvePending entry. Publishing that would be our own timeout recorded as the
// registry's answer — a verdict shown to users and counted in the status for a tag the
// registry was never given the time to answer about — so it must be reported as
// unexamined instead: no entry at all.
func TestFetchChartsOCIPublishesWhatAnExpiredPassExamined(t *testing.T) {
	// The probe timeout is deliberately far above the pass budget: this test is
	// about the whole pass expiring, not about one tag doing so.
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
	// A recorded verdict costs no request, so an expired budget is no reason to lose
	// it: dropping it would shrink the known state the next pass builds on.
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
	// The pass is reported as incomplete only because a tag was left over, not because
	// the registry was asked and failed to answer about it.
	if reg.manifestGets("1.0.1") == 0 {
		t.Fatal("the stalled tag must have been attempted at all")
	}
}

// TestFetchChartsOCIAbandonsThePassWhenTheParentDeadlineIsGone is the boundary of the
// test above: the pass survives its own budget, but not the caller's. passCtx.Err()
// cannot tell the two apart — the parent's deadline propagates into it — so the parent
// context has to be consulted directly, or a controller shutting down would publish a
// half-read catalog as a synchronization that advanced.
func TestFetchChartsOCIAbandonsThePassWhenTheParentDeadlineIsGone(t *testing.T) {
	// A pass budget far above the parent deadline: whatever ends this pass, it is not
	// the budget.
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

// TestFetchChartsOCIRecordsASingleStalledTagAsPending is the other side of the same
// boundary: one tag that stalls is a verdict about that tag, not a failed pass. The
// probe timeout expires well inside the pass budget, so the tag that answers is
// published and the stalled one is examined again by the next synchronization.
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

// TestFetchChartsOCIExaminesANeverSeenTagAheadOfAStalledOne pins the examination
// order, which is what makes repeated passes converge. Publishing what a pass reached
// is not enough on its own: a tag recorded as ResolvePending is examined again by
// every pass, so a block of permanently stalling tags at the head of the listing is
// re-probed first every time and spends the whole budget before the tags behind it are
// reached even once. Those tags would never be examined, however often the
// synchronization ran, and the published prefix would be exactly the part that keeps
// being re-probed.
//
// A tag with no recorded verdict has never been asked, while a pending one was asked
// and did not answer, so the never-asked ones go first: the set of tags nobody has a
// verdict for only shrinks, which is the guarantee that every tag is examined
// eventually. The second half of the test is the other side of it — the pending tags
// are not dropped from the pass, they are probed with the budget left after the new
// ones.
func TestFetchChartsOCIExaminesANeverSeenTagAheadOfAStalledOne(t *testing.T) {
	shortenOCILimits(t, tagListBudget, 200*time.Millisecond, time.Second)

	reg, host := newFakeRegistry(t)

	// More stalling tags than one budget can probe: eight concurrent probes of 200ms
	// each get through some forty of them in a second, so a tag listed behind all
	// eighty is out of reach of any pass that asks them first.
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

	// Stalled only after the fixtures are pushed: remote.Write reads manifests too.
	for _, tag := range stalled {
		reg.stallTag(tag)
	}

	// The first pass has no recorded verdict for anything, so every tag is a new one
	// and the listing order decides: the healthy tag sits behind eighty stalling ones
	// and is left unexamined. This is the situation the order has to get out of.
	first := fetchOne(t, host, FetchOptions{})
	if !slices.Contains(first.UnexaminedVersions, healthyTag) {
		t.Fatalf("the first pass was expected to run out of budget before %s; unexamined = %v",
			healthyTag, first.UnexaminedVersions)
	}

	// What the passes that probed the stalling tags left behind: a pending verdict
	// each, which NeedsExamination asks about again.
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

// TestFetchChartsOCIExaminesAMarkedTagAheadOfAStalledOne extends the ordering rule to
// the third group a pass can see. A tag whose recorded verdict is marked for
// re-examination has an answer, unlike a never seen one, but the answer is known to be
// out of date; a tag recorded as ResolvePending has no answer and is asked again by
// every pass, whether or not it ever replies. Ordered the other way round, a block of
// permanently stalling tags spends the whole budget before the marked ones are reached,
// and a forced refresh whose remainder they sit behind would never finish — which is
// the failure this whole mechanism exists to prevent. Marked ahead of stalled bounds
// it: the pass that examines a marked tag clears the mark, and nothing but a forced
// pass sets one, so the marked set only shrinks.
func TestFetchChartsOCIExaminesAMarkedTagAheadOfAStalledOne(t *testing.T) {
	shortenOCILimits(t, tagListBudget, 200*time.Millisecond, time.Second)

	reg, host := newFakeRegistry(t)

	// More stalling tags than one budget can probe, as in the never-seen case: eight
	// concurrent probes of 200ms get through some forty of them in a second.
	const (
		stallingTags = 80
		markedTag    = "9.0.0"
	)

	known := make(KnownVersions, stallingTags+1)

	for i := range stallingTags {
		tag := fmt.Sprintf("0.0.%d", i+1)
		pushChart(t, host, tag, helmConfigMediaType, currentChartMediaType)
		known[tag] = KnownVersion{UnavailableReason: helmv1alpha1.UnavailableReasonResolvePending}
	}

	pushChart(t, host, markedTag, helmConfigMediaType, currentChartMediaType)
	// Listed last and recorded with a media type: without the mark this tag would not
	// be requested at all, and with it it must be requested first.
	known[markedTag] = KnownVersion{MediaType: legacyChartMediaType, ReexaminationRequired: true}

	// Stalled only after the fixtures are pushed: remote.Write reads manifests too.
	for i := range stallingTags {
		reg.stallTag(fmt.Sprintf("0.0.%d", i+1))
	}
	reg.reset()

	chart := fetchOne(t, host, FetchOptions{Known: KnownCharts{"podinfo": known}})

	if got := versionByTag(t, chart, markedTag).MediaType; got != currentChartMediaType {
		t.Fatalf("%s media type is %q, want %q: a marked tag must be examined before the stalled ones",
			markedTag, got, currentChartMediaType)
	}

	probed := 0
	for i := range stallingTags {
		probed += reg.manifestGets(fmt.Sprintf("0.0.%d", i+1))
	}
	if probed == 0 {
		t.Fatal("the pending tags must still be re-examined with the budget left after the marked one")
	}
}

// TestFetchChartsOCIOrdersAForcedPassByWhatIsRecorded pins the one thing a forced pass
// must keep taking from the recorded state. Full says the verdicts are not to be
// trusted, so every listed tag is examined — but it says nothing about the order, and
// the order is what decides which tags a pass that runs out of its budget reaches.
// Read off "does this tag need a request", every tag of a forced pass falls into one
// group, the listing order decides, and the forced pass publishes an arbitrary prefix.
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

	// Listed last, recorded nowhere: a forced pass has to reach it just as an ordinary
	// one does, and only the recorded state can tell it apart from the eighty ahead.
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

// markAll is what forcing a refresh writes into the catalog before anything is asked
// of the registry (contract.Catalog.MarkForReexamination): every recorded verdict is
// now owed a fresh one.
func markAll(known KnownVersions) KnownVersions {
	marked := make(KnownVersions, len(known))
	for tag, version := range known {
		version.ReexaminationRequired = true
		marked[tag] = version
	}

	return marked
}

// nextKnown is the state the next pass starts from, the way the catalog builds it:
// every version this pass reached carries its fresh verdict and no mark, every version
// it did not reach keeps what was recorded for it, mark included.
// internal/catalog/merge.go owns this rule — TestAForcedPassKeepsTheMarkOnTheVersionsIt-
// DidNotReach pins it there, and TestSynchronizerSyncFinishesAForcedRefreshOverSeveral-
// IncompletePasses drives the real merge over a whole refresh — and it is restated here
// only so the two passes of the scenario below can be run against a real registry.
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

// TestFetchChartsOCIReexaminesARepushedTagAForcedPassCouldNotReach is the scenario the
// whole mechanism exists for, run against a registry over two passes.
//
// Someone re-pushes a tag the catalog already holds a verdict for. No ordinary pass
// will ever look at it again — a recorded media type is exactly what NeedsExamination
// reads as answered — so they force a refresh. The forced pass runs out of its budget
// among the tags ahead of it and never reaches the re-pushed one, which is where this
// used to end: the request was consumed, the next pass skipped the tag, and the catalog
// kept publishing the media type of an artifact that no longer exists, with nothing
// anywhere saying so.
//
// The pass records the mark instead, and the ordinary pass that follows examines the
// marked tag first and sees the new content.
func TestFetchChartsOCIReexaminesARepushedTagAForcedPassCouldNotReach(t *testing.T) {
	// A budget that admits at most two rounds of probes, against forty tags that never
	// answer: the re-pushed tag, listed last, is out of the forced pass's reach.
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

	// The registry holds the new content; the catalog still records what the tag
	// carried when it was last examined.
	pushChart(t, host, repushedTag, helmConfigMediaType, currentChartMediaType)
	known[repushedTag] = KnownVersion{MediaType: legacyChartMediaType}

	for i := range blockingTags {
		reg.stallTag(fmt.Sprintf("0.0.%d", i+1))
	}
	reg.reset()

	// Forcing the refresh records the request on every verdict the catalog holds, and
	// only then is the registry read.
	known = markAll(known)

	forced := fetchOne(t, host, FetchOptions{Known: KnownCharts{"podinfo": known}, Full: true})

	if !slices.Contains(forced.UnexaminedVersions, repushedTag) {
		t.Fatalf("the forced pass was expected to run out of budget before %s; unexamined = %v",
			repushedTag, forced.UnexaminedVersions)
	}

	known = nextKnown(known, forced)

	// The second pass is an ordinary one: the force annotation was consumed by the pass
	// above, and everything that is left to do is in the recorded state.
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
