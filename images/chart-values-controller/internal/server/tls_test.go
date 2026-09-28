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

const testWatchInterval = 20 * time.Millisecond

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

	awaitReload(t, reloaded, "second")

	if got := handshakeCommonName(t, addr); got != "second" {
		t.Fatalf("common name after rotation = %q, want %q", got, "second")
	}
}

func TestTLSConfigKeepsTheLoadedCertificateWhenTheFilesCannotBeReread(t *testing.T) {
	dir := t.TempDir()
	certFile := filepath.Join(dir, "tls.crt")
	keyFile := filepath.Join(dir, "tls.key")

	writeKeyPair(t, certFile, keyFile, "first")

	addr, _ := serveTLSConfig(t, certFile, keyFile)

	if got := handshakeCommonName(t, addr); got != "first" {
		t.Fatalf("common name = %q, want %q", got, "first")
	}

	failedReads := testutil.ToFloat64(metrics.ReadCertificateErrors)

	if err := os.WriteFile(certFile, []byte("-----BEGIN CERTIFICATE-----\nnot a certi"), 0o600); err != nil {
		t.Fatalf("truncate certificate: %v", err)
	}

	awaitFailedRead(t, failedReads)

	if got := handshakeCommonName(t, addr); got != "first" {
		t.Fatalf("common name after a failed re-read = %q, want the previous %q", got, "first")
	}
}

func TestTLSConfigKeepsServingAfterTheFilesDisappear(t *testing.T) {
	dir := t.TempDir()
	certFile := filepath.Join(dir, "tls.crt")
	keyFile := filepath.Join(dir, "tls.key")

	writeKeyPair(t, certFile, keyFile, "first")

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

func TestNewTLSConfigFailsWhenTheKeyPairCannotBeLoaded(t *testing.T) {
	dir := t.TempDir()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if _, err := newTLSConfig(ctx, filepath.Join(dir, "missing.crt"), filepath.Join(dir, "missing.key")); err == nil {
		t.Fatal("newTLSConfig with missing files = nil error, want an error")
	}
}

func serveTLSConfig(t *testing.T, certFile, keyFile string, opts ...watcherOption) (string, <-chan string) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

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

func awaitReload(t *testing.T, reloaded <-chan string, want string) {
	t.Helper()

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
