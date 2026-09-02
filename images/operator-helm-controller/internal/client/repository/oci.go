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
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Masterminds/semver/v3"
	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
	"golang.org/x/sync/errgroup"

	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
)

var OCIRepositoryDefaultClient ClientInterface = &ociRepositoryClient{}

type ociRepositoryClient struct{}

// chartResolveConcurrency bounds the manifest requests of one pass. The requests are
// small, but a repository with hundreds of new tags would otherwise open hundreds of
// connections at once.
const chartResolveConcurrency = 8

// unavailableMessageLimit keeps a registry error from bloating the chart status.
const unavailableMessageLimit = 256

func (c *ociRepositoryClient) FetchCharts(ctx context.Context, url string, config *RepoConfig, opts FetchOptions) ([]Chart, error) {
	url = trimSchemaPrefixes(url)
	url = strings.TrimSuffix(url, "/")

	if !strings.Contains(url, "/") {
		return nil, &TerminalError{
			Reason:  helmv1alpha1.ReasonInvalidRepositoryURL,
			Message: "repository url must contain the chart image name",
		}
	}

	urlParts := strings.Split(url, "/")
	chartName := urlParts[len(urlParts)-1]

	if len(chartName) == 0 {
		return nil, &TerminalError{
			Reason:  helmv1alpha1.ReasonInvalidRepositoryURL,
			Message: "cannot parse the chart image name from the repository url",
		}
	}

	repo, err := name.NewRepository(url)
	if err != nil {
		return nil, &TerminalError{
			Reason:  helmv1alpha1.ReasonInvalidRepositoryURL,
			Message: "cannot parse the repository url",
			Err:     err,
		}
	}

	options := []remote.Option{
		remote.WithContext(ctx),
		remote.WithUserAgent("operator-helm-controller"),
		remote.WithRetryBackoff(remote.Backoff{
			Duration: 1.0 * time.Second,
			Factor:   3.0,
			Jitter:   0.1,
			Steps:    3,
		}),
	}

	if config != nil && config.Username != "" {
		options = append(options, remote.WithAuth(authn.FromConfig(authn.AuthConfig{
			Username: config.Username,
			Password: config.Password,
		})))
	}

	if config != nil && (config.CACertificate != "" || config.Insecure) {
		options = append(options, remote.WithTransport(BuildTLSTransport(config)))
	}

	tags, err := remote.List(repo, options...)
	if err != nil {
		return nil, classifyRemoteError(err, url)
	}

	versions, err := resolveChartVersions(ctx, repo, chartName, tags, options, opts)
	if err != nil {
		return nil, err
	}

	return []Chart{
		{
			Name:     chartName,
			Versions: versions,
		},
	}, nil
}

// resolveChartVersions turns the listed tags into one version entry per tag. A tag
// whose verdict is already recorded is carried through without a request; the rest are
// examined concurrently. The only error returned is a terminal one: a per-tag failure
// becomes a ResolvePending entry so the rest of the pass is still published.
func resolveChartVersions(
	ctx context.Context,
	repo name.Repository,
	chartName string,
	tags []string,
	options []remote.Option,
	opts FetchOptions,
) ([]ChartVersion, error) {
	type candidate struct {
		tag     string
		version *semver.Version
		known   *KnownVersion
	}

	candidates := make([]candidate, 0, len(tags))

	for _, tag := range tags {
		if isCosignTag(tag) {
			continue
		}

		semVersion, err := semver.NewVersion(tag)
		if err != nil {
			continue
		}

		c := candidate{tag: tag, version: semVersion}
		if !opts.NeedsExamination(chartName, tag) {
			known := opts.Known[chartName][tag]
			c.known = &known
		}

		candidates = append(candidates, c)
	}

	resolved := make([]*ChartVersion, len(candidates))

	group, groupCtx := errgroup.WithContext(ctx)
	group.SetLimit(chartResolveConcurrency)

	// A fresh slice per pass: appending to the shared options slice from several
	// goroutines would race on its backing array.
	tagOptions := make([]remote.Option, 0, len(options)+2)
	tagOptions = append(tagOptions, options...)
	tagOptions = append(tagOptions,
		remote.WithContext(groupCtx),
		// One attempt per tag: a failure is recorded as pending and retried by the next
		// synchronization anyway, and retrying here would multiply the duration of a
		// pass over a degraded registry.
		remote.WithRetryBackoff(remote.Backoff{Duration: time.Millisecond, Factor: 1.0, Steps: 1}),
	)

	// One puller for the whole repository: it caches its fetcher (and therefore the
	// auth handshake) per repository behind a sync.Map/sync.Once and is safe for
	// concurrent use. remote.Get would build a fresh Puller per call, paying a fresh
	// /v2/ ping - and, against a bearer registry, a fresh token request - for every
	// examined tag.
	puller, err := remote.NewPuller(tagOptions...)
	if err != nil {
		return nil, fmt.Errorf("building the registry puller: %w", err)
	}

	for i := range candidates {
		index, c := i, candidates[i]

		group.Go(func() error {
			if c.known != nil {
				resolved[index] = carryKnown(c.version, *c.known)

				return nil
			}

			version, err := resolveChartVersion(groupCtx, puller, repo.Tag(c.tag), c.version)
			if err != nil {
				return err
			}
			resolved[index] = version

			return nil
		})
	}

	if err := group.Wait(); err != nil {
		return nil, err
	}

	// A cancelled parent context makes every remote.Get fail without surfacing as a
	// transport error, so every goroutine above would otherwise return a fabricated
	// ResolvePending verdict instead of an error. That is indistinguishable from a real
	// registry failure and must not be reported as a completed pass.
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	versions := make([]ChartVersion, 0, len(resolved))
	for _, version := range resolved {
		if version == nil {
			continue
		}
		versions = append(versions, *version)
	}

	return versions, nil
}

// carryKnown reuses a recorded verdict. A tag that is listed again is by definition no
// longer removed from the repository, so that marker - and the message describing the
// absence it no longer holds - is dropped here: presence in the listing is registry
// truth, which is this client's domain.
func carryKnown(version *semver.Version, known KnownVersion) *ChartVersion {
	reason, message := known.UnavailableReason, known.UnavailableMessage
	if reason == helmv1alpha1.UnavailableReasonRemovedFromRepository {
		reason, message = "", ""
	}

	return &ChartVersion{
		Version:            version,
		MediaType:          known.MediaType,
		UnavailableReason:  reason,
		UnavailableMessage: message,
	}
}

// resolveChartVersion examines one tag. A nil version with a nil error means the tag
// vanished between the listing and this request and must be treated as unlisted. A
// non-nil error is always terminal: credentials rejected for one tag are rejected for
// all of them, so there is no point in requesting the rest. puller is shared across all
// tags of the pass so the auth handshake happens once for the repository rather than
// once per tag.
func resolveChartVersion(ctx context.Context, puller *remote.Puller, ref name.Reference, version *semver.Version) (*ChartVersion, error) {
	desc, err := puller.Get(ctx, ref)
	if err != nil {
		var transportErr *transport.Error
		if errors.As(err, &transportErr) {
			switch transportErr.StatusCode {
			case http.StatusNotFound:
				return nil, nil
			case http.StatusUnauthorized, http.StatusForbidden:
				terminal := TerminalFromStatusCode(transportErr.StatusCode, ref.String())
				terminal.Err = err

				return nil, terminal
			}
		}

		return pendingVersion(version, err.Error()), nil
	}

	manifest, err := v1.ParseManifest(bytes.NewReader(desc.Manifest))
	if err != nil {
		// An unreadable manifest is a verdict about the artifact, not a transport
		// problem, so it is recorded the same way as an unsupported media type.
		return unsupportedVersion(version, "cannot parse the manifest: "+err.Error()), nil
	}

	verdict := examineManifest(desc.MediaType, manifest)
	if !verdict.OK() {
		return unsupportedVersion(version, verdict.Message), nil
	}

	return &ChartVersion{Version: version, MediaType: verdict.MediaType}, nil
}

func pendingVersion(version *semver.Version, message string) *ChartVersion {
	return &ChartVersion{
		Version:            version,
		UnavailableReason:  helmv1alpha1.UnavailableReasonResolvePending,
		UnavailableMessage: truncate(message),
	}
}

func unsupportedVersion(version *semver.Version, message string) *ChartVersion {
	return &ChartVersion{
		Version:            version,
		UnavailableReason:  helmv1alpha1.UnavailableReasonUnsupportedMediaType,
		UnavailableMessage: truncate(message),
	}
}

func truncate(message string) string {
	if len(message) <= unavailableMessageLimit {
		return message
	}

	cut := unavailableMessageLimit
	for cut > 0 && !utf8.RuneStart(message[cut]) {
		cut--
	}

	return message[:cut] + "…"
}

func trimSchemaPrefixes(url string) string {
	for _, prefix := range []string{"oci://", "http://", "https://"} {
		url = strings.TrimPrefix(url, prefix)
	}

	return url
}

func isCosignTag(tag string) bool {
	for _, suffix := range []string{".att", ".sbom", ".sig"} {
		if strings.HasSuffix(tag, suffix) {
			return true
		}
	}

	return false
}

// classifyRemoteError maps a registry rejection to a terminal error and leaves
// transport-level failures retriable. The original error is always wrapped so
// callers keep the full cause.
func classifyRemoteError(err error, url string) error {
	var transportErr *transport.Error
	if errors.As(err, &transportErr) {
		if terminal := TerminalFromStatusCode(transportErr.StatusCode, url); terminal != nil {
			terminal.Err = err

			return terminal
		}
	}

	return fmt.Errorf("listing image tags: %w", err)
}
