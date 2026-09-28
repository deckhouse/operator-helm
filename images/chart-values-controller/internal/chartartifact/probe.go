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

// Package chartartifact reads an OCI artifact's manifest to find the layer that holds a
// packaged Helm chart, against the media types the API module defines.
package chartartifact

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"

	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
)

// ErrNotAChart means the artifact behind the tag was read but is not a packaged Helm
// chart. It is a verdict about the artifact: retrying cannot change it.
var ErrNotAChart = errors.New("artifact is not a packaged helm chart")

// ErrTagNotFound means the repository index offers a tag the registry does not have.
// Also a verdict rather than a transient failure.
var ErrTagNotFound = errors.New("registry has no such tag")

// ProbeTimeout bounds one probe end to end, including when the caller's context carries no deadline.
const ProbeTimeout = 20 * time.Second

func probeContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, ProbeTimeout)
}

// Prober finds the chart layer of one OCI artifact.
type Prober interface {
	ChartLayerMediaType(ctx context.Context, ref string, transport http.RoundTripper) (string, error)
}

// Default is the Prober used in production.
var Default Prober = registryProber{}

type registryProber struct{}

// ChartLayerMediaType reads the manifest behind ref and returns the media type of the layer
// holding the chart. No credentials are sent; rt carries transport settings only and may be nil.
func (registryProber) ChartLayerMediaType(ctx context.Context, ref string, rt http.RoundTripper) (string, error) {
	ctx, cancel := probeContext(ctx)
	defer cancel()

	// A transport built by Transport is single-use, so its pool is drained here.
	if closer, ok := rt.(interface{ CloseIdleConnections() }); ok {
		defer closer.CloseIdleConnections()
	}

	tag, err := name.NewTag(strings.TrimPrefix(ref, "oci://"))
	if err != nil {
		return "", fmt.Errorf("chart reference %q cannot be parsed: %w", ref, err)
	}

	options := []remote.Option{
		remote.WithContext(ctx),
		remote.WithUserAgent("chart-values-controller"),
		remote.WithRetryBackoff(remote.Backoff{Duration: time.Second, Factor: 2.0, Jitter: 0.1, Steps: 2}),
	}
	if rt != nil {
		options = append(options, remote.WithTransport(rt))
	}

	desc, err := remote.Get(tag, options...)
	if err != nil {
		var transportErr *transport.Error
		if errors.As(err, &transportErr) && transportErr.StatusCode == http.StatusNotFound {
			return "", fmt.Errorf("%w: %s", ErrTagNotFound, ref)
		}

		return "", fmt.Errorf("reading the manifest of %s: %w", ref, err)
	}

	if desc.MediaType.IsIndex() {
		return "", fmt.Errorf("%w: %s points at an index (%s)", ErrNotAChart, ref, desc.MediaType)
	}

	manifest, err := v1.ParseManifest(bytes.NewReader(desc.Manifest))
	if err != nil {
		return "", fmt.Errorf("%w: cannot parse the manifest of %s: %w", ErrNotAChart, ref, err)
	}

	if !helmv1alpha1.IsChartConfigMediaType(string(manifest.Config.MediaType)) {
		return "", fmt.Errorf("%w: config media type of %s is %q", ErrNotAChart, ref, manifest.Config.MediaType)
	}

	for _, supported := range helmv1alpha1.ChartLayerMediaTypes {
		for _, layer := range manifest.Layers {
			if string(layer.MediaType) == supported {
				return supported, nil
			}
		}
	}

	return "", fmt.Errorf("%w: %s has no supported chart layer", ErrNotAChart, ref)
}

// ErrInvalidCACertificate means the repository's caCertificate holds no PEM encoded
// certificate. Like the verdicts above it cannot be resolved by trying again.
var ErrInvalidCACertificate = errors.New(
	`caCertificate holds no PEM encoded certificate (it is the same value the operator mirrors into the "ca.crt" key of the repository's internal tls secret)`,
)

// Transport builds the transport settings for reaching a registry the repository names, as a clone
// of http.DefaultTransport, or nil when it has none. It is built per probe, whose caller closes it.
func Transport(caCertificate string, insecure bool) (http.RoundTripper, error) {
	if caCertificate == "" && !insecure {
		return nil, nil
	}

	tlsConfig := &tls.Config{InsecureSkipVerify: insecure} //nolint:gosec // opt-in via the repository spec

	if caCertificate != "" {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM([]byte(caCertificate)) {
			return nil, ErrInvalidCACertificate
		}

		tlsConfig.RootCAs = pool
	}

	rt := http.DefaultTransport.(*http.Transport).Clone()
	rt.TLSClientConfig = tlsConfig

	return rt, nil
}
