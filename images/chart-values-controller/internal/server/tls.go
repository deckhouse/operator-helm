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
	"crypto/tls"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/certwatcher"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// newTLSConfig builds the TLS configuration the API is served with, so that a
// certificate reissued by the tls-certificates-controller hook is picked up
// without restarting the process: the pair is served from memory through
// GetCertificate and reloaded when the mounted files change.
//
// The reload is controller-runtime's certwatcher — the same machinery the
// validating webhook of operator-helm-controller gets from the manager's
// webhook server, which serves its certificate out of a mounted Secret the
// same way. It watches the two paths with fsnotify and re-reads them on a
// 10s poll as a backstop, so a handshake never touches the disk, a pair
// caught half-written fails tls.X509KeyPair and is logged and dropped
// instead of replacing the working one, and the next event or poll picks up
// the complete pair.
//
// The watcher runs until ctx is done; an initial read that fails is fatal,
// because there is no previous pair to fall back to and the API could not be
// served at all.
func newTLSConfig(ctx context.Context, certFile, keyFile string) (*tls.Config, error) {
	watcher, err := certwatcher.New(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("load TLS key pair: %w", err)
	}

	go func() {
		if err := watcher.Start(ctx); err != nil {
			log.FromContext(ctx).Error(err, "TLS certificate watcher stopped",
				"cert", certFile, "key", keyFile)
		}
	}()

	return &tls.Config{
		GetCertificate: watcher.GetCertificate,
		MinVersion:     tls.VersionTLS12,
	}, nil
}
