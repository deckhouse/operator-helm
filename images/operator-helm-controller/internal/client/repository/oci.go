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

const chartResolveConcurrency = 8 // Bounds the manifest requests in flight during one pass.

const unavailableMessageLimit = 256 // Keeps a registry error from bloating the chart status.

// Variables rather than constants so the tests that pin them need not wait out real minutes.
var (
	tagListBudget          = time.Minute      // Bounds the whole listing: its pages, its internal retries and their pauses.
	chartProbeTimeout      = 15 * time.Second // Bounds one manifest request.
	chartResolvePassBudget = 3 * time.Minute  // Bounds the examination pass; what it reached is published, the rest reported unexamined.
)

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
		tlsTransport, err := BuildTLSTransport(config)
		if err != nil {
			return nil, err
		}

		options = append(options, remote.WithTransport(tlsTransport))
	}

	listCtx, cancelList := context.WithTimeout(ctx, tagListBudget)
	defer cancelList()

	// A fresh slice: the listing's deadline must not leak into the per-tag options. The later WithContext wins.
	listOptions := make([]remote.Option, 0, len(options)+1)
	listOptions = append(listOptions, options...)
	listOptions = append(listOptions, remote.WithContext(listCtx))

	tags, err := remote.List(repo, listOptions...)
	if err != nil {
		return nil, classifyRemoteError(err, url)
	}

	versions, unexamined, err := resolveChartVersions(ctx, repo, chartName, tags, options, opts)
	if err != nil {
		return nil, err
	}

	return []Chart{
		{
			Name:               chartName,
			Versions:           versions,
			UnexaminedVersions: unexamined,
		},
	}, nil
}

func resolveChartVersions(
	ctx context.Context,
	repo name.Repository,
	chartName string,
	tags []string,
	options []remote.Option,
	opts FetchOptions,
) ([]ChartVersion, []string, error) {
	type candidate struct {
		tag     string
		version *semver.Version
		known   *KnownVersion
		// recorded says a previous pass wrote a verdict for this tag, whatever that verdict was.
		recorded bool
		// stale says a forced refresh marked the recorded verdict and no pass has answered it yet.
		stale bool
		// relisted says the verdict records the tag as removed while this listing offers it again.
		relisted bool
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

		known, recorded := opts.Known[chartName][tag]

		c := candidate{
			tag:      tag,
			version:  semVersion,
			recorded: recorded,
			stale:    recorded && known.ReexaminationRequired,
			relisted: recorded && known.UnavailableReason == helmv1alpha1.UnavailableReasonRemovedFromRepository,
		}
		if !opts.NeedsExamination(chartName, tag) {
			c.known = &known
		}

		candidates = append(candidates, c)
	}

	// Order matters: never seen tags, then stale, then relisted, then unanswered — only the last never empties.
	probeOrder := make([]int, 0, len(candidates))

	for _, group := range []func(candidate) bool{
		func(c candidate) bool { return !c.recorded },
		func(c candidate) bool { return c.stale },
		func(c candidate) bool { return c.relisted && !c.stale },
		func(c candidate) bool { return c.recorded && !c.stale && !c.relisted },
	} {
		for i := range candidates {
			if candidates[i].known == nil && group(candidates[i]) {
				probeOrder = append(probeOrder, i)
			}
		}
	}

	resolved := make([]*ChartVersion, len(candidates))
	// A nil in resolved is also the verdict for a vanished tag, so examined tells the two apart.
	examined := make([]bool, len(candidates))

	// passCtx, not groupCtx: errgroup cancels its own context on any failure and as Wait returns.
	passCtx, cancelPass := context.WithTimeout(ctx, chartResolvePassBudget)
	defer cancelPass()

	group, groupCtx := errgroup.WithContext(passCtx)
	group.SetLimit(chartResolveConcurrency)

	// A fresh slice per pass: appending to the shared options from several goroutines would race.
	tagOptions := make([]remote.Option, 0, len(options)+2)
	tagOptions = append(tagOptions, options...)
	tagOptions = append(tagOptions,
		remote.WithContext(groupCtx),
		// One attempt per tag: a failure is recorded as pending and retried by the next pass.
		remote.WithRetryBackoff(remote.Backoff{Duration: time.Millisecond, Factor: 1.0, Steps: 1}),
	)

	// One puller for the whole repository: it caches the auth handshake, where remote.Get would not.
	puller, err := remote.NewPuller(tagOptions...)
	if err != nil {
		return nil, nil, fmt.Errorf("building the registry puller: %w", err)
	}

	// Carried outside the group: a recorded verdict costs no request and must survive an expired budget.
	for i := range candidates {
		if candidates[i].known != nil {
			resolved[i], examined[i] = carryKnown(candidates[i].version, *candidates[i].known), true
		}
	}

	for _, index := range probeOrder {
		c := candidates[index]

		group.Go(func() error {
			// The budget is already spent: every probe from here on would fail on a cancelled context.
			if passCtx.Err() != nil {
				return nil
			}

			// One tag that stalls must cost only its own deadline, not the slot every queued tag waits for.
			probeCtx, cancelProbe := context.WithTimeout(groupCtx, chartProbeTimeout)
			defer cancelProbe()

			version, err := resolveChartVersion(probeCtx, puller, repo.Tag(c.tag), c.version)
			if err != nil {
				return err
			}

			// A probe cut short by our own budget is reported as unexamined, not as the registry's answer.
			if version != nil &&
				version.UnavailableReason == helmv1alpha1.UnavailableReasonResolvePending &&
				passCtx.Err() != nil {
				return nil
			}

			resolved[index], examined[index] = version, true

			return nil
		})
	}

	if err := group.Wait(); err != nil {
		return nil, nil, err
	}

	// The parent context, not passCtx: a cancelled parent leaves nobody to write the partial catalog.
	if err := ctx.Err(); err != nil {
		return nil, nil, fmt.Errorf("examining the versions of %s: %w", chartName, err)
	}

	versions := make([]ChartVersion, 0, len(resolved))

	var unexamined []string

	for i := range candidates {
		switch {
		case !examined[i]:
			unexamined = append(unexamined, candidates[i].version.Original())
		case resolved[i] != nil:
			versions = append(versions, *resolved[i])
		}
	}

	return versions, unexamined, nil
}

func carryKnown(version *semver.Version, known KnownVersion) *ChartVersion {
	return &ChartVersion{
		Version:            version,
		MediaType:          known.MediaType,
		UnavailableReason:  known.UnavailableReason,
		UnavailableMessage: known.UnavailableMessage,
	}
}

func resolveChartVersion(ctx context.Context, puller *remote.Puller, ref name.Reference, version *semver.Version) (*ChartVersion, error) {
	desc, err := puller.Get(ctx, ref)
	if err != nil {
		var transportErr *transport.Error
		if errors.As(err, &transportErr) {
			switch transportErr.StatusCode {
			case http.StatusNotFound:
				// The tag vanished between the listing and this request: nil/nil means unlisted.
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
		// An unreadable manifest is a verdict about the artifact, not a transport problem.
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
