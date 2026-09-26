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

package server

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestTLSConfigServesTheRotatedCertificateWithoutARestart is the point of the
// watcher: the hook reissues the pair into the mounted Secret while the process
// keeps running, and the next handshake has to present the new certificate.
func TestTLSConfigServesTheRotatedCertificateWithoutARestart(t *testing.T) {
	dir := t.TempDir()
	certFile := filepath.Join(dir, "tls.crt")
	keyFile := filepath.Join(dir, "tls.key")

	writeKeyPair(t, certFile, keyFile, "first")

	addr := serveTLSConfig(t, certFile, keyFile)

	if got := handshakeCommonName(t, addr); got != "first" {
		t.Fatalf("common name before rotation = %q, want %q", got, "first")
	}

	writeKeyPair(t, certFile, keyFile, "second")

	if got := awaitCommonName(t, addr, "second"); got != "second" {
		t.Fatalf("common name after rotation = %q, want %q", got, "second")
	}
}

// TestTLSConfigKeepsTheLoadedCertificateWhenTheFilesCannotBeReread covers the
// moment a rotation is caught half-written, and any other unreadable state: the
// re-read fails, and the server has to keep answering with the pair it already
// holds instead of failing handshakes.
func TestTLSConfigKeepsTheLoadedCertificateWhenTheFilesCannotBeReread(t *testing.T) {
	dir := t.TempDir()
	certFile := filepath.Join(dir, "tls.crt")
	keyFile := filepath.Join(dir, "tls.key")

	writeKeyPair(t, certFile, keyFile, "first")

	addr := serveTLSConfig(t, certFile, keyFile)

	if got := handshakeCommonName(t, addr); got != "first" {
		t.Fatalf("common name = %q, want %q", got, "first")
	}

	// A certificate truncated mid-write parses as nothing at all, which is what
	// the watcher must refuse to install.
	if err := os.WriteFile(certFile, []byte("-----BEGIN CERTIFICATE-----\nnot a certi"), 0o600); err != nil {
		t.Fatalf("truncate certificate: %v", err)
	}

	// The watcher reacts to the write asynchronously, so give it room to fail
	// before asserting that nothing changed.
	time.Sleep(200 * time.Millisecond)

	if got := handshakeCommonName(t, addr); got != "first" {
		t.Fatalf("common name after a failed re-read = %q, want the previous %q", got, "first")
	}
}

// TestTLSConfigDoesNotReadTheFilesOnEveryHandshake pins that the pair is served
// from memory: reading both files on the handshake path would put a disk access
// in front of every request. With the files gone, handshakes that touched the
// disk could not succeed.
func TestTLSConfigDoesNotReadTheFilesOnEveryHandshake(t *testing.T) {
	dir := t.TempDir()
	certFile := filepath.Join(dir, "tls.crt")
	keyFile := filepath.Join(dir, "tls.key")

	writeKeyPair(t, certFile, keyFile, "first")

	addr := serveTLSConfig(t, certFile, keyFile)

	if err := os.Remove(certFile); err != nil {
		t.Fatalf("remove certificate: %v", err)
	}
	if err := os.Remove(keyFile); err != nil {
		t.Fatalf("remove key: %v", err)
	}

	for range 3 {
		if got := handshakeCommonName(t, addr); got != "first" {
			t.Fatalf("common name without the files on disk = %q, want %q", got, "first")
		}
	}
}

// TestNewTLSConfigFailsWhenTheKeyPairCannotBeLoaded pins the one case that is
// fatal: without a first successful read there is no pair to serve at all.
func TestNewTLSConfigFailsWhenTheKeyPairCannotBeLoaded(t *testing.T) {
	dir := t.TempDir()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if _, err := newTLSConfig(ctx, filepath.Join(dir, "missing.crt"), filepath.Join(dir, "missing.key")); err == nil {
		t.Fatal("newTLSConfig with missing files = nil error, want an error")
	}
}

// serveTLSConfig starts a TLS listener configured the way the API server is and
// returns its address. Only the handshake matters here, so the connection is
// closed as soon as it is accepted.
func serveTLSConfig(t *testing.T, certFile, keyFile string) string {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	cfg, err := newTLSConfig(ctx, certFile, keyFile)
	if err != nil {
		t.Fatalf("newTLSConfig: %v", err)
	}

	ln, err := tls.Listen("tcp", "127.0.0.1:0", cfg)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			_ = conn.(*tls.Conn).HandshakeContext(ctx)
			_ = conn.Close()
		}
	}()

	return ln.Addr().String()
}

// handshakeCommonName dials addr and reports the common name of the certificate
// the server presented.
func handshakeCommonName(t *testing.T, addr string) string {
	t.Helper()

	conn, err := tls.Dial("tcp", addr, &tls.Config{
		InsecureSkipVerify: true, //nolint:gosec // the test asserts on the presented certificate itself, not on a chain
		ServerName:         "localhost",
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()

	certs := conn.ConnectionState().PeerCertificates
	if len(certs) == 0 {
		t.Fatal("server presented no certificate")
	}

	return certs[0].Subject.CommonName
}

// awaitCommonName polls until the server presents want, or the deadline passes;
// the watcher reloads asynchronously, so the change cannot be observed
// synchronously after the write.
func awaitCommonName(t *testing.T, addr, want string) string {
	t.Helper()

	var got string
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		got = handshakeCommonName(t, addr)
		if got == want {
			return got
		}
		time.Sleep(50 * time.Millisecond)
	}

	return got
}

// writeKeyPair writes a fresh self-signed certificate with the given common
// name, and its key, to the two paths. The key is written first so the pair is
// only ever incomplete in the direction a rotation would leave it.
func writeKeyPair(t *testing.T, certFile, keyFile, commonName string) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	template := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: commonName},
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}

	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}

	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}

	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(keyFile, keyPEM, 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := os.WriteFile(certFile, certPEM, 0o600); err != nil {
		t.Fatalf("write certificate: %v", err)
	}
}
