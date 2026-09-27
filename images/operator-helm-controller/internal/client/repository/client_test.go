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

	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
)

// testServerCACertificate returns the PEM encoding of the certificate a TLS test
// server presents, which is the shape a repository's spec.caCertificate carries.
func testServerCACertificate(t *testing.T, server *httptest.Server) string {
	t.Helper()

	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}))
}

// TestBuildTLSTransportTrustsAValidCACertificate pins that a well-formed
// certificate still produces a pool that actually verifies the remote: the check
// added for the malformed case must not cost the working one. Verification is
// proven by a real handshake rather than by inspecting the pool, because an empty
// pool is indistinguishable from a populated one by shape alone — that is exactly
// what made the unchecked append invisible.
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

// TestBuildTLSTransportRejectsACACertificateWithoutAnyCertificate pins the fault
// the unchecked append used to swallow. Both shapes reach an empty pool — bytes
// that are not PEM at all, and PEM that carries something other than a certificate
// — and both must be reported as terminal with the reason that names the field,
// because no number of retries can parse them.
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

// TestFetchChartsRejectsAnUnparsableCACertificate pins the propagation: the
// verdict has to leave the client as a terminal error, because that is what
// reposync turns into a terminal fetch outcome and the repository into Stalled. A
// transport error swallowed inside the fetch would be retried at the sync cadence
// instead.
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

// TestResolveChartArtifactRejectsAnUnparsableCACertificate pins the same verdict on
// the other user of the transport: a version examined at deploy time must fail the
// same way, rather than reporting the artifact unreadable for a fault that is in
// the repository's own spec.
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
