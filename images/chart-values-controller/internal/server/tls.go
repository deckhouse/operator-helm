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
	"errors"
	"fmt"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/certwatcher"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// watcherOption tunes the certificate watcher before it is started; the server takes the defaults and only tests pass one.
type watcherOption func(*certwatcher.CertWatcher)

// withWatchInterval replaces the 10s poll backstop.
func withWatchInterval(interval time.Duration) watcherOption {
	return func(watcher *certwatcher.CertWatcher) {
		_ = watcher.WithWatchInterval(interval)
	}
}

// withReloadCallback reports every pair the watcher installs, starting with the one it read while being constructed.
func withReloadCallback(callback func(tls.Certificate)) watcherOption {
	return func(watcher *certwatcher.CertWatcher) {
		watcher.RegisterCallback(callback)
	}
}

// newTLSConfig serves the key pair from memory through GetCertificate, reloaded by controller-runtime's
// certwatcher (fsnotify plus a 10s poll) so a reissued certificate is picked up without a restart.
func newTLSConfig(ctx context.Context, certFile, keyFile string, opts ...watcherOption) (*tls.Config, error) {
	watcher, err := certwatcher.New(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("load TLS key pair: %w", err)
	}

	for _, opt := range opts {
		opt(watcher)
	}

	go func() {
		// A shutdown landing while Start is still registering the file watches comes back as a cancelled context, which is the normal way the process stops.
		if err := watcher.Start(ctx); err != nil && !errors.Is(err, context.Canceled) {
			log.FromContext(ctx).Error(err, "TLS certificate watcher stopped",
				"cert", certFile, "key", keyFile)
		}
	}()

	return &tls.Config{
		GetCertificate: watcher.GetCertificate,
		MinVersion:     tls.VersionTLS12,
	}, nil
}
