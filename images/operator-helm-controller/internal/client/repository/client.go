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
	// UnexaminedVersions names the versions the repository offers that this pass
	// reached no verdict about at all, because it ran out of its own budget before
	// getting to them. They are neither usable nor unavailable: nothing was observed
	// about them, so they carry no entry in Versions and the catalog leaves whatever a
	// previous pass recorded for them untouched. A client that always answers in full
	// leaves this empty, and a non-empty value is what tells the caller the pass was
	// incomplete and has to be repeated.
	UnexaminedVersions []string
}

type ChartVersion struct {
	Version *semver.Version
	IconURL string

	// MediaType is the OCI media type of the layer holding this chart version. It is
	// empty for helm repositories and for OCI versions that are not usable.
	MediaType string
	// OCIRef is the reference the repository index publishes this version at, set
	// only when the index points it at a registry rather than at a chart archive.
	// The tag is always explicit.
	OCIRef string
	// UnavailableReason and UnavailableMessage are set when the version cannot be
	// deployed and are empty for a usable one.
	UnavailableReason  string
	UnavailableMessage string
	// Carried says this entry is the verdict a previous pass recorded, reused without
	// asking the registry anything, because NeedsExamination answered no for the tag.
	// It is there so the catalog goes on publishing the verdict, not because this pass
	// established it, and the distinction is invisible in the fields above — a carried
	// entry and a freshly probed one that reached the same verdict are identical. The
	// catalog merge needs it to tell an answer from an echo: the re-examination mark is
	// cleared by an answer only. This is a client-side field with no counterpart in the
	// API types; nothing about it is ever written to the cluster.
	Carried bool
}

// KnownVersion is the verdict a previous pass reached for one tag.
type KnownVersion struct {
	MediaType          string
	UnavailableReason  string
	UnavailableMessage string
	// ReexaminationRequired says the recorded verdict is stale: a forced refresh was
	// asked for and did not reach this tag. It is a verdict about the verdict, so it
	// travels next to it rather than in FetchOptions.
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

// NeedsExamination reports whether a listed tag has to be examined again. A pending
// entry is examined whatever else it records: ResolvePending is not a verdict but the
// absence of one, and the media type next to it is the one the catalog merge carries
// forward to keep a version still in use deployable, so reading it as "already
// examined" would freeze the probe that never reached a verdict. A tag whose entry is
// marked for re-examination is asked for the same reason and in the same way: the
// verdict next to the mark is the one a reader still deploys from, and the mark says
// it is no longer trusted. A tag recorded as removed from the repository is asked too:
// the entry survives only because a consumer references it, and the listing this is
// asked about offers the tag again, so there is new content behind a verdict that was
// reached about content that was deleted. Otherwise a recorded media type is authoritative; an
// unsupported artifact is a verdict and is not re-examined until a full pass; anything
// else — never seen, or seen without a verdict (an entry written before media types
// were recorded) — is examined.
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

// ChartResolverInterface examines one OCI tag and reports the media type of the
// layer that holds the chart. It is separate from ClientInterface because it serves a
// different question at a different time: ClientInterface reads a catalog on the
// repository's schedule, this one answers "is this exact artifact a chart" when an
// addon is about to be deployed.
type ChartResolverInterface interface {
	// ResolveChartArtifact returns the media type of the chart layer behind ref. A
	// TerminalError means the verdict will not change by retrying — the artifact is
	// not a chart, or the tag does not exist; any other error is worth another
	// attempt.
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

// BuildTLSTransport builds the transport a repository's TLS settings describe.
//
// A caCertificate that holds no certificate is reported instead of being ignored.
// AppendCertsFromPEM says only whether it added anything, and a pool it added
// nothing to is a valid, empty trust root: every read then fails with "x509:
// certificate signed by unknown authority", which names the remote rather than the
// field, and is classified as an ordinary read failure and retried forever against
// bytes no attempt can parse. The error is terminal for that reason — only an edit
// of the repository can resolve it.
//
// The timeouts mirror go-containerregistry's remote.DefaultTransport, which this
// transport replaces for a repository that configures TLS: without them a repository
// that merely pins a CA would be strictly worse off than one that does not, because a
// bare &http.Transport{} bounds nothing at all — not even the dial. ResponseHeaderTimeout
// is the one addition: the default transport leaves it unset, and it is the gap a
// registry that accepts the connection and then goes silent falls through. The calls
// themselves carry deadlines too (see oci.go), but those cannot bound a connection
// that is reused for the next request.
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
			// A registry whose address does not accept connections must not hold a
			// reconcile worker for longer than it takes to learn that.
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2: true,
		MaxIdleConns:      100,
		// Two hosts at most (the registry and its token service), so the idle pool is
		// split between them, as the default transport does.
		MaxIdleConnsPerHost: 50,
		IdleConnTimeout:     90 * time.Second,
		// A TLS handshake that stalls after the TCP connect is the failure a pinned CA
		// or a misconfigured registry produces most often.
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		// Not in the default transport, and the reason this whole block exists: a
		// registry that completes the handshake and then never sends a status line
		// holds the request until something else gives up. Thirty seconds is above any
		// manifest or tag-listing latency worth waiting for and below every call budget
		// in this package, so the call's own deadline stays the thing that decides.
		ResponseHeaderTimeout: 30 * time.Second,
	}, nil
}
