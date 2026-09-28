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
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/Masterminds/semver/v3"

	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
	"github.com/deckhouse/operator-helm/internal/chartsource"
)

type Chart struct {
	Name     string
	Versions []ChartVersion
	// UnexaminedVersions names the versions the repository offers that this pass reached no
	// verdict about, because it ran out of its own budget; they carry no entry in Versions.
	UnexaminedVersions []string
}

type ChartVersion struct {
	Version *semver.Version
	IconURL string

	// MediaType is the OCI media type of the layer holding this chart version. It is
	// empty for helm repositories and for OCI versions that are not usable.
	MediaType string
	// OCIRef is the reference the repository index publishes this version at, set only when
	// the index points it at a registry rather than at a chart archive. The tag is explicit.
	OCIRef string
	// UnavailableReason and UnavailableMessage are set when the version cannot be
	// deployed and are empty for a usable one.
	UnavailableReason  string
	UnavailableMessage string
}

// KnownVersion is the verdict a previous pass reached for one tag.
type KnownVersion struct {
	MediaType          string
	UnavailableReason  string
	UnavailableMessage string
	// ReexaminationRequired says the recorded verdict is stale: a forced refresh was asked
	// for and did not reach this tag.
	ReexaminationRequired bool
}

// KnownVersions maps a tag to its recorded verdict.
type KnownVersions map[string]KnownVersion

// KnownCharts maps a chart name to the tags already examined for it.
type KnownCharts map[string]KnownVersions

// FetchOptions carries the incremental state of the previous pass, so a client can
// skip the tags whose verdict is already recorded.
type FetchOptions struct {
	Known KnownCharts
	Full  bool
}

// NeedsExamination reports whether a listed tag has to be examined again. A recorded media
// type or unsupported verdict is authoritative; a pending, marked or removed entry is not.
func (o FetchOptions) NeedsExamination(chartName, tag string) bool {
	if o.Full {
		return true
	}

	known, recorded := o.Known[chartName][tag]
	switch {
	case !recorded:
		return true
	case known.ReexaminationRequired:
		return true
	case known.UnavailableReason == helmv1alpha1.UnavailableReasonResolvePending:
		return true
	case known.UnavailableReason == helmv1alpha1.UnavailableReasonRemovedFromRepository:
		return true
	case known.MediaType != "":
		return false
	default:
		return known.UnavailableReason != helmv1alpha1.UnavailableReasonUnsupportedMediaType
	}
}

type ClientInterface interface {
	FetchCharts(ctx context.Context, url string, config *RepoConfig, opts FetchOptions) ([]Chart, error)
}

// ChartResolverInterface answers "is this exact artifact a chart" for one OCI tag, where
// ClientInterface reads a whole catalog on the repository's schedule.
type ChartResolverInterface interface {
	// ResolveChartArtifact returns the media type of the chart layer behind ref. A TerminalError
	// means the verdict will not change by retrying; any other error is worth another attempt.
	ResolveChartArtifact(ctx context.Context, ref string, config *RepoConfig) (string, error)
}

func NewClient(repoType chartsource.Kind) (ClientInterface, error) {
	switch repoType {
	case chartsource.Helm:
		return HelmRepositoryDefaultClient, nil
	case chartsource.OCI:
		return OCIRepositoryDefaultClient, nil
	default:
		return nil, fmt.Errorf("unknown repository type: %s", repoType)
	}
}

type RepoConfig struct {
	Username      string
	Password      string
	CACertificate string
	Insecure      bool
}

// BuildTLSTransport builds the transport a repository's TLS settings describe. A caCertificate
// holding no PEM certificate is a TerminalError rather than a valid but empty trust root.
func BuildTLSTransport(config *RepoConfig) (*http.Transport, error) {
	tlsConfig := &tls.Config{}

	if config.Insecure {
		tlsConfig.InsecureSkipVerify = true
	}

	if config.CACertificate != "" {
		caCertPool := x509.NewCertPool()
		if !caCertPool.AppendCertsFromPEM([]byte(config.CACertificate)) {
			return nil, &TerminalError{
				Reason: helmv1alpha1.ReasonInvalidCACertificate,
				Message: "spec.caCertificate holds no PEM encoded certificate " +
					`(it is mirrored verbatim into the "ca.crt" key of the repository's internal tls secret)`,
			}
		}

		tlsConfig.RootCAs = caCertPool
	}

	return &http.Transport{
		TLSClientConfig: tlsConfig,
		Proxy:           http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   50,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		// Not in the default transport: a registry that never sends a status line would hold the request.
		ResponseHeaderTimeout: 30 * time.Second,
	}, nil
}
