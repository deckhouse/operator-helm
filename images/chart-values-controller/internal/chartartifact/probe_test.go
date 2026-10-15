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
	"errors"
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

func TestChartLayerMediaTypeGivesUpOnAServerThatNeverAnswers(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	t.Cleanup(server.Close)

	host := strings.TrimPrefix(server.URL, "http://")

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

func TestChartLayerMediaTypeWithCustomTLSSettings(t *testing.T) {
	server := httptest.NewTLSServer(registry.New(registry.Logger(log.New(io.Discard, "", 0))))
	t.Cleanup(server.Close)

	host := strings.TrimPrefix(server.URL, "https://")

	pushTestChart(t, host, server.Client().Transport)

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})

	rt, err := Transport(string(certPEM), false)
	if err != nil {
		t.Fatalf("building a transport from the registry's own certificate: %v", err)
	}
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

func TestTransportClonesDefaultTimeouts(t *testing.T) {
	rt, err := Transport("", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

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

func TestTransportRejectsACACertificateWithoutAnyCertificate(t *testing.T) {
	tests := []struct {
		name          string
		caCertificate string
	}{
		{name: "garbage bytes", caCertificate: "not a certificate at all"},
		{name: "truncated pem block", caCertificate: "-----BEGIN CERTIFICATE-----\nMIIBhzCCAS2gAwIBAgIQ"},
		{
			name: "pem block that is not a certificate",
			caCertificate: string(pem.EncodeToMemory(&pem.Block{
				Type:  "PRIVATE KEY",
				Bytes: []byte("a key where a certificate is expected"),
			})),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rt, err := Transport(test.caCertificate, false)
			if err == nil {
				t.Fatalf("transport %v was built from a CA certificate that carries none", rt)
			}
			if !errors.Is(err, ErrInvalidCACertificate) {
				t.Fatalf("error = %v, want it to wrap ErrInvalidCACertificate", err)
			}
			if rt != nil {
				t.Fatal("no round tripper may be returned alongside the error")
			}
		})
	}
}

type deadlineTransport struct {
	deadline time.Time
	bounded  bool
}

func (d *deadlineTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	d.deadline, d.bounded = req.Context().Deadline()

	return nil, errors.New("the registry is not reached in this test")
}

func TestChartLayerMediaTypeBoundsAnUnboundedCaller(t *testing.T) {
	rt := &deadlineTransport{}
	if _, err := Default.ChartLayerMediaType(context.Background(), "oci://registry.example/podinfo:1.0.0", rt); err == nil {
		t.Fatal("expected the probe to fail on a transport that never reaches the registry")
	}

	if !rt.bounded {
		t.Fatal("a probe given no deadline must still be bounded by ProbeTimeout")
	}
	if remaining := time.Until(rt.deadline); remaining <= 0 || remaining > ProbeTimeout {
		t.Fatalf("deadline is %s from now, want within (0, %s]", remaining, ProbeTimeout)
	}
}

func TestChartLayerMediaTypeKeepsAnEarlierCallerDeadline(t *testing.T) {
	parent, cancelParent := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelParent()

	rt := &deadlineTransport{}
	if _, err := Default.ChartLayerMediaType(parent, "oci://registry.example/podinfo:1.0.0", rt); err == nil {
		t.Fatal("expected the probe to fail on a transport that never reaches the registry")
	}

	parentDeadline, _ := parent.Deadline()
	if !rt.deadline.Equal(parentDeadline) {
		t.Fatalf("deadline = %s, want the caller's own earlier deadline %s", rt.deadline, parentDeadline)
	}
}

type spyTransport struct {
	http.RoundTripper
	closed int
}

func (s *spyTransport) CloseIdleConnections() {
	s.closed++
}

func TestChartLayerMediaTypeClosesTheTransportItWasGiven(t *testing.T) {
	server := httptest.NewTLSServer(registry.New(registry.Logger(log.New(io.Discard, "", 0))))
	t.Cleanup(server.Close)

	host := strings.TrimPrefix(server.URL, "https://")

	pushTestChart(t, host, server.Client().Transport)

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	rt, err := Transport(string(certPEM), false)
	if err != nil {
		t.Fatalf("building a transport from the registry's own certificate: %v", err)
	}

	spy := &spyTransport{RoundTripper: rt}

	if _, err := Default.ChartLayerMediaType(context.Background(), "oci://"+host+"/podinfo:1.0.0", spy); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if spy.closed != 1 {
		t.Fatalf("CloseIdleConnections called %d times, want exactly 1: the pool must not outlive this single probe", spy.closed)
	}
}

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
