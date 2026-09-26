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

	"github.com/prometheus/client_golang/prometheus/testutil"
	"sigs.k8s.io/controller-runtime/pkg/certwatcher/metrics"
)

// testWatchInterval is the poll backstop the tests run the watcher with. The
// watcher registers its file watches asynchronously, so a write that lands
// before the registration is only ever seen by the next poll; at the production
// 10s interval that turns every rotation assertion into a race with the clock.
const testWatchInterval = 20 * time.Millisecond

// TestTLSConfigServesTheRotatedCertificateWithoutARestart is the point of the
// watcher: the hook reissues the pair into the mounted Secret while the process
// keeps running, and the next handshake has to present the new certificate.
func TestTLSConfigServesTheRotatedCertificateWithoutARestart(t *testing.T) {
	dir := t.TempDir()
	certFile := filepath.Join(dir, "tls.crt")
	keyFile := filepath.Join(dir, "tls.key")

	writeKeyPair(t, certFile, keyFile, "first")

	addr, reloaded := serveTLSConfig(t, certFile, keyFile)

	if got := handshakeCommonName(t, addr); got != "first" {
		t.Fatalf("common name before rotation = %q, want %q", got, "first")
	}

	writeKeyPair(t, certFile, keyFile, "second")

	// The watcher installs a pair before it reports it, so once the reload is
	// observed the next handshake has to present it.
	awaitReload(t, reloaded, "second")

	if got := handshakeCommonName(t, addr); got != "second" {
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

	addr, _ := serveTLSConfig(t, certFile, keyFile)

	if got := handshakeCommonName(t, addr); got != "first" {
		t.Fatalf("common name = %q, want %q", got, "first")
	}

	// The watcher counts every read it could not turn into a pair, which is the
	// observable proof that it did try to pick the broken file up.
	failedReads := testutil.ToFloat64(metrics.ReadCertificateErrors)

	// A certificate truncated mid-write parses as nothing at all, which is what
	// the watcher must refuse to install.
	if err := os.WriteFile(certFile, []byte("-----BEGIN CERTIFICATE-----\nnot a certi"), 0o600); err != nil {
		t.Fatalf("truncate certificate: %v", err)
	}

	awaitFailedRead(t, failedReads)

	if got := handshakeCommonName(t, addr); got != "first" {
		t.Fatalf("common name after a failed re-read = %q, want the previous %q", got, "first")
	}
}

// TestTLSConfigKeepsServingAfterTheFilesDisappear covers the mounted pair going
// away under a running process — an unmount, or a rotation that unlinks before
// it writes: the server has to keep completing handshakes with the certificate
// it already holds instead of failing them.
func TestTLSConfigKeepsServingAfterTheFilesDisappear(t *testing.T) {
	dir := t.TempDir()
	certFile := filepath.Join(dir, "tls.crt")
	keyFile := filepath.Join(dir, "tls.key")

	writeKeyPair(t, certFile, keyFile, "first")

	// Nothing here waits for a reload, and with the files gone every poll would
	// only log a failed read, so keep the backstop out of the way.
	addr, _ := serveTLSConfig(t, certFile, keyFile, withWatchInterval(time.Hour))

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
// returns its address together with the common name of every pair the watcher
// installs, the first of which is the one read at construction. Only the
// handshake matters here, so the connection is closed as soon as it is
// accepted.
func serveTLSConfig(t *testing.T, certFile, keyFile string, opts ...watcherOption) (string, <-chan string) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	// Buffered and never blocking: the watcher reports a reload from its own
	// goroutine, and a test that stops reading must not wedge it.
	reloaded := make(chan string, 8)
	opts = append([]watcherOption{
		withWatchInterval(testWatchInterval),
		withReloadCallback(func(cert tls.Certificate) {
			select {
			case reloaded <- installedCommonName(cert):
			default:
			}
		}),
	}, opts...)

	cfg, err := newTLSConfig(ctx, certFile, keyFile, opts...)
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

	return ln.Addr().String(), reloaded
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

// awaitReload blocks until the watcher reports that it installed the pair with
// the given common name. The watcher reloads asynchronously, so this is what a
// test waits on instead of guessing how long the reload takes.
func awaitReload(t *testing.T, reloaded <-chan string, want string) {
	t.Helper()

	// Long enough that only a watcher that never reloads at all can hit it: the
	// poll backstop alone answers within testWatchInterval.
	timeout := time.NewTimer(time.Minute)
	defer timeout.Stop()

	for {
		select {
		case got := <-reloaded:
			if got == want {
				return
			}
		case <-timeout.C:
			t.Fatalf("the watcher did not install the certificate of %q", want)
		}
	}
}

// awaitFailedRead blocks until the watcher has counted a read it could not turn
// into a key pair on top of the reads it had already counted.
func awaitFailedRead(t *testing.T, before float64) {
	t.Helper()

	deadline := time.Now().Add(time.Minute)
	for time.Now().Before(deadline) {
		if testutil.ToFloat64(metrics.ReadCertificateErrors) > before {
			return
		}
		time.Sleep(time.Millisecond)
	}

	t.Fatal("the watcher never tried to re-read the broken key pair")
}

// installedCommonName reports the common name of a pair the watcher installed.
// It runs on the watcher's own goroutine, where a failed assertion would be
// illegal, so an unparsable leaf simply names no certificate and the waiting
// test keeps waiting for one it recognises.
func installedCommonName(cert tls.Certificate) string {
	leaf := cert.Leaf
	if leaf == nil {
		parsed, err := x509.ParseCertificate(cert.Certificate[0])
		if err != nil {
			return ""
		}
		leaf = parsed
	}

	return leaf.Subject.CommonName
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
