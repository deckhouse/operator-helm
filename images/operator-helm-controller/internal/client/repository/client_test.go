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
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/go-containerregistry/pkg/v1/remote"

	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
)

func TestBuildTLSTransportKeepsTheBoundsOfTheDefaultTransport(t *testing.T) {
	built, err := BuildTLSTransport(&RepoConfig{Insecure: true})
	if err != nil {
		t.Fatalf("BuildTLSTransport returned %v", err)
	}

	standard, ok := remote.DefaultTransport.(*http.Transport)
	if !ok {
		t.Fatalf("remote.DefaultTransport is %T, the comparison below assumes *http.Transport", remote.DefaultTransport)
	}

	if built.DialContext == nil {
		t.Fatal("a custom-TLS transport must dial through a bounded dialer, not the zero-value one")
	}

	if built.TLSHandshakeTimeout != standard.TLSHandshakeTimeout {
		t.Fatalf("TLSHandshakeTimeout = %v, want the default transport's %v", built.TLSHandshakeTimeout, standard.TLSHandshakeTimeout)
	}
	if built.IdleConnTimeout != standard.IdleConnTimeout {
		t.Fatalf("IdleConnTimeout = %v, want the default transport's %v", built.IdleConnTimeout, standard.IdleConnTimeout)
	}
	if built.ExpectContinueTimeout != standard.ExpectContinueTimeout {
		t.Fatalf("ExpectContinueTimeout = %v, want the default transport's %v", built.ExpectContinueTimeout, standard.ExpectContinueTimeout)
	}
	if built.MaxIdleConns != standard.MaxIdleConns || built.MaxIdleConnsPerHost != standard.MaxIdleConnsPerHost {
		t.Fatalf("idle pool = %d/%d, want the default transport's %d/%d",
			built.MaxIdleConns, built.MaxIdleConnsPerHost, standard.MaxIdleConns, standard.MaxIdleConnsPerHost)
	}

	if built.ResponseHeaderTimeout == 0 {
		t.Fatal("a silent registry must be abandoned by the transport as well: ResponseHeaderTimeout is unset")
	}
}

func TestNeedsExaminationPerRecordedVerdict(t *testing.T) {
	const mediaType = "application/vnd.cncf.helm.chart.content.v1.tar+gzip"

	tests := []struct {
		name  string
		known KnownVersions
		full  bool
		want  bool
	}{
		{name: "a tag no pass has seen", known: KnownVersions{}, want: true},
		{name: "a resolved tag", known: KnownVersions{"1.0.0": {MediaType: mediaType}}, want: false},
		{
			name:  "an unsupported artifact",
			known: KnownVersions{"1.0.0": {UnavailableReason: helmv1alpha1.UnavailableReasonUnsupportedMediaType}},
			want:  false,
		},
		{
			name:  "a failed probe",
			known: KnownVersions{"1.0.0": {UnavailableReason: helmv1alpha1.UnavailableReasonResolvePending}},
			want:  true,
		},
		{
			name: "a failed probe on a version in use, whose media type the merge carried forward",
			known: KnownVersions{"1.0.0": {
				MediaType:          mediaType,
				UnavailableReason:  helmv1alpha1.UnavailableReasonResolvePending,
				UnavailableMessage: "GET https://registry.example.invalid/v2/: dial tcp: i/o timeout",
			}},
			want: true,
		},
		{
			name:  "a version the repository removed and now lists again",
			known: KnownVersions{"1.0.0": {MediaType: mediaType, UnavailableReason: helmv1alpha1.UnavailableReasonRemovedFromRepository}},
			want:  true,
		},
		{name: "an entry written before media types were recorded", known: KnownVersions{"1.0.0": {}}, want: true},
		{
			name:  "a full pass ignores every verdict",
			known: KnownVersions{"1.0.0": {MediaType: mediaType}},
			full:  true,
			want:  true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			opts := FetchOptions{Known: KnownCharts{"podinfo": test.known}, Full: test.full}

			if got := opts.NeedsExamination("podinfo", "1.0.0"); got != test.want {
				t.Fatalf("NeedsExamination = %t, want %t", got, test.want)
			}
		})
	}
}

func testServerCACertificate(t *testing.T, server *httptest.Server) string {
	t.Helper()

	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}))
}

func TestBuildTLSTransportTrustsAValidCACertificate(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)

	transport, err := BuildTLSTransport(&RepoConfig{CACertificate: testServerCACertificate(t, server)})
	if err != nil {
		t.Fatalf("building a transport from the server's own certificate: %v", err)
	}
	if transport.TLSClientConfig.RootCAs == nil {
		t.Fatal("a valid CA certificate must be installed as the trust root")
	}

	client := &http.Client{Transport: transport}

	resp, err := client.Get(server.URL)
	if err != nil {
		t.Fatalf("request over a transport trusting the server's CA: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusNoContent)
	}
}

func TestBuildTLSTransportRejectsACACertificateWithoutAnyCertificate(t *testing.T) {
	tests := []struct {
		name          string
		caCertificate string
	}{
		{
			name:          "garbage bytes",
			caCertificate: "not a certificate at all",
		},
		{
			name:          "truncated pem block",
			caCertificate: "-----BEGIN CERTIFICATE-----\nMIIBhzCCAS2gAwIBAgIQ",
		},
		{
			name: "pem block that is not a certificate",
			caCertificate: string(pem.EncodeToMemory(&pem.Block{
				Type:  "PRIVATE KEY",
				Bytes: []byte("a key where a certificate is expected"),
			})),
		},
		{
			name: "certificate block holding something other than der",
			caCertificate: string(pem.EncodeToMemory(&pem.Block{
				Type:  "CERTIFICATE",
				Bytes: []byte("not der"),
			})),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			transport, err := BuildTLSTransport(&RepoConfig{CACertificate: test.caCertificate})
			if err == nil {
				t.Fatalf("transport %+v was built from a CA certificate that carries none", transport)
			}

			terminal, ok := AsTerminal(err)
			if !ok {
				t.Fatalf("error %v is not terminal, so the repository would retry it forever", err)
			}
			if terminal.Reason != helmv1alpha1.ReasonInvalidCACertificate {
				t.Fatalf("reason = %q, want %q", terminal.Reason, helmv1alpha1.ReasonInvalidCACertificate)
			}
			if terminal.Message == "" {
				t.Fatal("the message must name what is wrong, it is the only thing a user sees")
			}
		})
	}
}

func TestFetchChartsRejectsAnUnparsableCACertificate(t *testing.T) {
	client := &helmRepositoryClient{}

	_, err := client.FetchCharts(
		context.Background(),
		"https://charts.example.invalid",
		&RepoConfig{CACertificate: "-----BEGIN CERTIFICATE-----\ntruncated"},
		FetchOptions{},
	)
	if err == nil {
		t.Fatal("expected the fetch to fail before contacting the repository")
	}

	terminal, ok := AsTerminal(err)
	if !ok {
		t.Fatalf("error %v is not terminal", err)
	}
	if terminal.Reason != helmv1alpha1.ReasonInvalidCACertificate {
		t.Fatalf("reason = %q, want %q", terminal.Reason, helmv1alpha1.ReasonInvalidCACertificate)
	}
}

func TestResolveChartArtifactRejectsAnUnparsableCACertificate(t *testing.T) {
	resolver := &ociChartResolver{}

	_, err := resolver.ResolveChartArtifact(
		context.Background(),
		"oci://registry.example.invalid/charts/podinfo:1.0.0",
		&RepoConfig{CACertificate: "not a certificate at all"},
	)
	if err == nil {
		t.Fatal("expected the resolve to fail before contacting the registry")
	}

	terminal, ok := AsTerminal(err)
	if !ok {
		t.Fatalf("error %v is not terminal", err)
	}
	if terminal.Reason != helmv1alpha1.ReasonInvalidCACertificate {
		t.Fatalf("reason = %q, want %q", terminal.Reason, helmv1alpha1.ReasonInvalidCACertificate)
	}
}

func TestBuildTLSTransportLetsInsecureOutweighTheCA(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()

	ca := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}))

	built, err := BuildTLSTransport(&RepoConfig{CACertificate: ca, Insecure: true})
	if err != nil {
		t.Fatalf("BuildTLSTransport returned %v", err)
	}
	if !built.TLSClientConfig.InsecureSkipVerify {
		t.Error("a spec that asked for no verification must not have one put back by its caCertificate")
	}
}
