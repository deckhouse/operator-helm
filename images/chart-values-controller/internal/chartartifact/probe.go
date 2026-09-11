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

// Package chartartifact reads an OCI artifact's manifest to find the layer that holds
// a packaged Helm chart.
//
// A chart version that a helm repository's index publishes in a registry carries no
// recorded media type: the operator resolves it when an addon is deployed and does not
// persist it, so this service has to look for itself before it can build the source
// object that fetches values.yaml. What counts as a chart is not decided here — the
// media types come from the API module, so this service and the operator cannot drift
// into disagreeing about the same artifact.
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

// Prober finds the chart layer of one OCI artifact.
type Prober interface {
	ChartLayerMediaType(ctx context.Context, ref string, transport http.RoundTripper) (string, error)
}

// Default is the Prober used in production.
var Default Prober = registryProber{}

type registryProber struct{}

// ChartLayerMediaType reads the manifest behind ref and returns the media type of the
// layer holding the chart.
//
// No credentials are ever sent: a version published through a third-party index is
// pulled anonymously by the source object this answer feeds, so a probe that
// authenticated would report a chart the pull could not fetch. rt carries transport
// settings only and may be nil.
func (registryProber) ChartLayerMediaType(ctx context.Context, ref string, rt http.RoundTripper) (string, error) {
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

// Transport builds the transport settings for reaching a registry the repository
// itself names. It returns nil when the repository has nothing to say about the
// transport, which leaves the caller on the default one.
func Transport(caCertificate string, insecure bool) http.RoundTripper {
	if caCertificate == "" && !insecure {
		return nil
	}

	tlsConfig := &tls.Config{InsecureSkipVerify: insecure} //nolint:gosec // opt-in via the repository spec

	if caCertificate != "" {
		pool := x509.NewCertPool()
		pool.AppendCertsFromPEM([]byte(caCertificate))
		tlsConfig.RootCAs = pool
	}

	return &http.Transport{TLSClientConfig: tlsConfig}
}
