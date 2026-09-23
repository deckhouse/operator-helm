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

package chartartifact

import (
	"context"
	"encoding/pem"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/static"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

// TestChartLayerMediaTypeGivesUpOnAServerThatNeverAnswers pins the second half of
// the probe's bound: whatever deadline the caller's context carries, the whole
// probe — dial through body read — must end when that deadline passes rather
// than hang on a registry that never answers.
func TestChartLayerMediaTypeGivesUpOnAServerThatNeverAnswers(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		// Never respond; only give up when the request's own context ends, the
		// same way a hung connection to a real registry would behave.
		<-r.Context().Done()
	}))
	t.Cleanup(server.Close)

	host := strings.TrimPrefix(server.URL, "http://")

	// The deadline is set on the caller's context, not read from probeTimeout:
	// the point of this test is that the probe respects whatever bound it is
	// given, including one shorter than its own floor.
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := Default.ChartLayerMediaType(ctx, "oci://"+host+"/podinfo:1.0.0", nil)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected an error from a server that never answers")
	}
	if elapsed > 5*time.Second {
		t.Fatalf("probe took %s to give up on a 200ms deadline: it is not bounded by the caller's context", elapsed)
	}
}

// TestChartLayerMediaTypeWithCustomTLSSettings pins the other half: a transport
// built by Transport for a registry with its own CA must still let the probe
// succeed, proving the clone in Transport carries a usable TLS configuration
// rather than a broken bare one.
func TestChartLayerMediaTypeWithCustomTLSSettings(t *testing.T) {
	server := httptest.NewTLSServer(registry.New(registry.Logger(log.New(io.Discard, "", 0))))
	t.Cleanup(server.Close)

	host := strings.TrimPrefix(server.URL, "https://")

	pushTestChart(t, host, server.Client().Transport)

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})

	rt := Transport(string(certPEM), false)
	if rt == nil {
		t.Fatal("Transport must return a round tripper when a CA certificate is given")
	}

	mediaType, err := Default.ChartLayerMediaType(context.Background(), "oci://"+host+"/podinfo:1.0.0", rt)
	if err != nil {
		t.Fatalf("probing over a transport trusting the registry's CA: %v", err)
	}
	if mediaType != "application/vnd.cncf.helm.chart.content.v1.tar+gzip" {
		t.Fatalf("media type = %q, want the chart layer's type", mediaType)
	}
}

// TestTransportClonesDefaultTimeouts pins the first half of the bound: a
// transport built for one probe must carry the configured default's dial and
// idle-connection timeouts, not the zero values of a bare &http.Transport{}.
func TestTransportClonesDefaultTimeouts(t *testing.T) {
	rt := Transport("", true)

	transport, ok := rt.(*http.Transport)
	if !ok {
		t.Fatalf("Transport must return an *http.Transport, got %T", rt)
	}

	want := http.DefaultTransport.(*http.Transport) //nolint:forcetypeassert // http.DefaultTransport is always *http.Transport
	if transport.IdleConnTimeout != want.IdleConnTimeout {
		t.Fatalf("IdleConnTimeout = %v, want the default's %v: a bare transport carries none of its timeouts", transport.IdleConnTimeout, want.IdleConnTimeout)
	}
	if transport.TLSHandshakeTimeout != want.TLSHandshakeTimeout {
		t.Fatalf("TLSHandshakeTimeout = %v, want %v", transport.TLSHandshakeTimeout, want.TLSHandshakeTimeout)
	}
	if transport.TLSClientConfig == nil || !transport.TLSClientConfig.InsecureSkipVerify {
		t.Fatal("the clone must still carry the requested TLS configuration")
	}
}

// TestProbeContextBoundsAnUnboundedCaller is the fast, deterministic half of the
// deadline test: a caller that supplies no deadline at all (context.Background(),
// exactly what a reconcile loop passes) must still get one from probeTimeout,
// without this test having to wait for a real hang.
func TestProbeContextBoundsAnUnboundedCaller(t *testing.T) {
	ctx, cancel := probeContext(context.Background())
	defer cancel()

	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("a probe given no deadline must still be bounded by probeTimeout")
	}
	if remaining := time.Until(deadline); remaining <= 0 || remaining > probeTimeout {
		t.Fatalf("deadline is %s from now, want within (0, %s]", remaining, probeTimeout)
	}
}

// TestProbeContextKeepsAnEarlierCallerDeadline pins that probeContext narrows
// but never widens: a caller deadline sooner than probeTimeout must still win.
func TestProbeContextKeepsAnEarlierCallerDeadline(t *testing.T) {
	parent, cancelParent := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancelParent()

	ctx, cancel := probeContext(parent)
	defer cancel()

	parentDeadline, _ := parent.Deadline()
	deadline, _ := ctx.Deadline()
	if !deadline.Equal(parentDeadline) {
		t.Fatalf("deadline = %s, want the caller's own earlier deadline %s", deadline, parentDeadline)
	}
}

// spyTransport counts how many times its idle connections are closed, so a test
// can pin that a single-use transport is drained exactly once per probe.
type spyTransport struct {
	http.RoundTripper
	closed int
}

func (s *spyTransport) CloseIdleConnections() {
	s.closed++
}

// TestChartLayerMediaTypeClosesTheTransportItWasGiven pins the other half of the
// transport fix: Transport builds a single-use pool for one probe, so the probe
// must drain it once finished rather than leaking it for the life of the process.
func TestChartLayerMediaTypeClosesTheTransportItWasGiven(t *testing.T) {
	server := httptest.NewTLSServer(registry.New(registry.Logger(log.New(io.Discard, "", 0))))
	t.Cleanup(server.Close)

	host := strings.TrimPrefix(server.URL, "https://")

	pushTestChart(t, host, server.Client().Transport)

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	spy := &spyTransport{RoundTripper: Transport(string(certPEM), false)}

	if _, err := Default.ChartLayerMediaType(context.Background(), "oci://"+host+"/podinfo:1.0.0", spy); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if spy.closed != 1 {
		t.Fatalf("CloseIdleConnections called %d times, want exactly 1: the pool must not outlive this single probe", spy.closed)
	}
}

// pushTestChart writes a single-layer chart artifact to host/podinfo:1.0.0 using
// a transport that trusts the test registry's certificate.
func pushTestChart(t *testing.T, host string, rt http.RoundTripper) {
	t.Helper()

	const helmConfigMediaType = types.MediaType("application/vnd.cncf.helm.config.v1+json")
	const chartLayerMediaType = types.MediaType("application/vnd.cncf.helm.chart.content.v1.tar+gzip")

	img, err := mutate.Append(empty.Image, mutate.Addendum{
		Layer:     static.NewLayer([]byte("chart-1.0.0"), chartLayerMediaType),
		MediaType: chartLayerMediaType,
	})
	if err != nil {
		t.Fatalf("appending layer: %v", err)
	}
	img = mutate.MediaType(img, types.OCIManifestSchema1)
	img = mutate.ConfigMediaType(img, helmConfigMediaType)

	ref, err := name.NewTag(host + "/podinfo:1.0.0")
	if err != nil {
		t.Fatalf("parsing tag: %v", err)
	}
	if err := remote.Write(ref, img, remote.WithTransport(rt)); err != nil {
		t.Fatalf("pushing chart: %v", err)
	}
}
