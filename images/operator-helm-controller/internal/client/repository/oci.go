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

// The bounds below are variables rather than constants for the same reason as the
// ones in helm.go: the tests that pin them would otherwise have to wait out real
// minutes. Nothing outside this package writes them.
//
// They exist because neither the reconcile context nor go-containerregistry supplies
// a deadline of its own: the default transport bounds the dial and the TLS handshake
// but not the wait for a response, so a registry that accepts the connection and then
// says nothing holds one of the repository controller's two workers forever, with the
// repository already reporting a synchronization in progress.
var (
	// tagListBudget bounds the whole listing: remote.List is one logical read that
	// may page, and it retries internally (three steps, ~1s+3s of backoff), so the
	// budget has to cover the pages, the attempts and the pauses together — which is
	// why it is a budget and not a per-request timeout. A minute matches
	// indexFetchBudget: this is the OCI equivalent of reading index.yaml, it runs
	// once per SyncInterval (5 minutes), and a listing still unfinished after a
	// minute is a remote worth failing and retrying rather than waiting on.
	tagListBudget = time.Minute
	// chartProbeTimeout bounds one manifest request. A manifest is a few kilobytes
	// that a healthy registry answers in milliseconds; fifteen seconds is the same
	// allowance indexAttemptTimeout makes for a single request, and a tag that needs
	// more is recorded as pending and examined again by the next pass.
	chartProbeTimeout = 15 * time.Second
	// chartResolvePassBudget bounds the examination pass as a whole. It has to exist
	// separately from chartProbeTimeout: with chartResolveConcurrency probes in
	// flight and hundreds of tags queued behind them, per-probe deadlines alone still
	// multiply into hours. An expired budget is not a failed pass: what was examined
	// is published and the rest is reported as unexamined, so a repository with more
	// tags than one budget admits converges over several passes instead of never.
	// Three minutes is the compromise between how much one pass gets through and how
	// long a stalled registry may hold a worker: it gives up well inside the 5-minute
	// SyncInterval instead of starving the repositories queued behind this worker.
	chartResolvePassBudget = 3 * time.Minute
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

	// A fresh slice: the listing's deadline must not leak into the options the
	// per-tag puller is built from. The later WithContext wins.
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

// resolveChartVersions turns the listed tags into one version entry per tag, plus the
// names of the tags it never got to. A tag whose verdict is already recorded is
// carried through without a request; the rest are examined concurrently. A per-tag
// failure becomes a ResolvePending entry, not a returned error, so the rest of the
// pass is still published.
//
// Running out of the pass budget is not an error either: the versions examined before
// it expired are returned, the rest are reported as unexamined, and the caller repeats
// the pass. A repository with more tags than one budget can cover would otherwise never
// converge — every pass would die at the same point, having published nothing, so every
// next pass would start from the same empty known state.
//
// Publishing what a pass reached is only half of that convergence; the other half is the
// order the tags are examined in, which is why it is not the listing order. A tag nobody
// has a verdict for has never been asked, while a tag recorded as ResolvePending was
// asked and did not answer — and NeedsExamination asks it again on every pass. Taken in
// listing order, a block of permanently stalling tags at the head of the listing is
// re-probed first by every pass and spends the whole budget before the tags behind it
// are reached even once, so those tags are never examined however often the pass is
// repeated. Examining the never-seen tags first bounds that: the set of tags with no
// verdict only shrinks, so every tag the repository offers is examined within a finite
// number of passes, and the stalling ones are re-probed with whatever budget is left
// after them rather than instead of them. A tag whose recorded verdict is marked for
// re-examination goes between the two, for the same reason: the mark is cleared by the
// pass that examines the tag and set by nothing but forcing a refresh, so that set only
// shrinks too, while a stalling tag is asked again for as long as it keeps stalling.
//
// resolveChartVersions returns an error in three cases, none of them a per-tag verdict,
// and all of them a failure of the whole pass rather than of one tag:
//   - building the shared puller fails: a transport/auth setup step that happens once
//     for the repository, not per tag, so its failure cannot be attributed to any tag;
//   - a per-tag request is rejected with 401/403: credentials rejected for one tag are
//     rejected for all of them, so resolveChartVersion escalates it to a terminal
//     error instead of a ResolvePending verdict, and group.Wait propagates it here;
//   - the parent context is cancelled — the controller is shutting down or the reconcile
//     deadline is gone. That is not our budget running out, nothing is waiting to publish
//     the partial catalog, and the versions still in flight were cut short by it, so the
//     pass is abandoned rather than reported.
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
		// recorded says a previous pass wrote a verdict for this tag, whatever that
		// verdict was. It is not the same question as known != nil: a ResolvePending
		// entry is recorded and still has to be examined again, and it is exactly that
		// combination the examination order has to put last.
		recorded bool
		// stale says the recorded verdict carries the mark a forced refresh writes onto
		// everything the catalog holds before the registry is read, and that no pass has
		// answered yet. Such a tag has an answer, unlike a never seen one, but the answer
		// is known to be out of date, unlike a stalling one.
		stale bool
		// relisted says the recorded verdict is that the repository no longer offers the
		// tag, while this listing offers it again. The listing is itself the evidence that
		// the recorded verdict is out of date, so it says what the mark says without any
		// mark having to be written — and it says it about an entry whose media type is the
		// deleted artifact's, which a consumer is still deploying from.
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

	// probeOrder is the order the tags that need a request are asked in: the ones no pass
	// has a verdict for first, then the ones whose recorded verdict a forced pass marked
	// as stale, then the ones recorded as gone that this listing offers again, and last
	// the ones a previous pass left without an answer. Within each group the listing order
	// is kept, so two passes over an unchanged repository ask in the same order — the
	// convergence must not depend on which tags a pass happened to get to.
	//
	// The last group is last because it is the only one that can never empty: a tag the
	// registry permanently stalls on is re-examined by every pass forever, and a block of
	// them ahead of anything else would spend every budget on work that cannot finish.
	// Everything ordered ahead of it is bounded — a never seen tag is recorded once, a
	// mark is cleared by the pass that answers it, and a relisted tag stops being recorded
	// as removed as soon as it is examined — so each group drains and the ones behind it
	// get their turn.
	//
	// A relisted tag needs a priority of its own rather than the mark's: it is examined on
	// the strength of the listing alone (NeedsExamination), so it can be unmarked — a
	// forced refresh that already ran, or none ever asked for — and left among the stalling
	// retries it would never be reached behind them, while the catalog goes on publishing
	// the media type of an artifact that was deleted and replaced.
	//
	// The groups are read off what is recorded, never off whether this pass examines a
	// tag: under Full every tag needs a request, and ordering by that would put the whole
	// listing in one group and leave the order to the registry. Reading them off the
	// recorded state is what keeps the four kinds of work apart even then, so a forced pass
	// that runs out of its budget stops somewhere in that order rather than somewhere in
	// the registry's.
	//
	// The candidates themselves are left in listing order: they index resolved and
	// examined below, and the versions this function returns are built from them.
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
	// examined records which candidates this pass reached a verdict about, which a nil
	// entry in resolved cannot: nil is also the answer for a tag that vanished between
	// the listing and its probe, and that one is a verdict — the tag is gone and the
	// catalog must drop it — while an unexamined tag must be left exactly as the
	// previous pass recorded it. Each goroutine writes only its own index.
	examined := make([]bool, len(candidates))

	// passCtx, not groupCtx, is the one consulted inside the goroutines: errgroup
	// cancels its own context as Wait returns, and also as soon as any goroutine fails,
	// so groupCtx cannot tell an expired budget from a sibling's terminal error.
	passCtx, cancelPass := context.WithTimeout(ctx, chartResolvePassBudget)
	defer cancelPass()

	group, groupCtx := errgroup.WithContext(passCtx)
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
		return nil, nil, fmt.Errorf("building the registry puller: %w", err)
	}

	// Carried outside the group: a recorded verdict costs no request, and a pass whose
	// budget expires must still publish every one of them. A carry queued behind stalled
	// probes would be left undone, its verdict would drop out of the catalog, and the
	// next pass would have to examine that tag from scratch — the incremental state
	// would shrink instead of growing.
	for i := range candidates {
		if candidates[i].known != nil {
			resolved[i], examined[i] = carryKnown(candidates[i].version, *candidates[i].known), true
		}
	}

	for _, index := range probeOrder {
		c := candidates[index]

		group.Go(func() error {
			// The budget is already spent: every probe from here on would fail on a
			// cancelled context, and behind a stalled registry the queue is as long as the
			// tag list. They are unexamined either way, so do not ask.
			if passCtx.Err() != nil {
				return nil
			}

			// One tag that stalls must cost only its own deadline: without this the
			// slot it holds is unavailable to every tag still queued behind it, and
			// chartResolveConcurrency of them would stall the pass until its budget.
			//
			// The puller builds its fetcher on whichever probe gets there first and
			// caches the outcome, the error included, so a /v2/ ping or token exchange
			// that outlasts this deadline is remembered as that probe's failure and
			// handed to every later tag at once: a catalog of pending verdicts under a
			// pass budget that never expired. Initialising it here instead would cost a
			// manifest request of its own and turn a tag that merely stalls into a
			// failed pass. The state corrects itself — a pending version is examined
			// again on the next synchronization — and the transport's response-header
			// timeout keeps the handshake well inside this deadline in the first place.
			probeCtx, cancelProbe := context.WithTimeout(groupCtx, chartProbeTimeout)
			defer cancelProbe()

			version, err := resolveChartVersion(probeCtx, puller, repo.Tag(c.tag), c.version)
			if err != nil {
				return err
			}

			// A probe cut short by our own budget (or by the parent context) fails inside
			// remote.Get without any transport error, so resolveChartVersion cannot tell it
			// from a registry that answered badly and builds a ResolvePending entry for it.
			// Recording that would publish our own timeout as the registry's answer: the
			// catalog would show, and the status would count, a version the registry was
			// never given the time to say anything about. Reporting it as unexamined is the
			// honest answer and the one the next pass acts on — a tag with no verdict at all
			// is examined ahead of the tags that already have one. Only a pending verdict is
			// suspect: a tag that answered just before the budget expired was genuinely
			// examined and is kept.
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

	// The parent context, not passCtx: passCtx is also done when our own budget expired,
	// which is the case this function now survives. A cancelled parent is something else
	// — the controller is shutting down or the reconcile deadline is gone — and nobody is
	// left to write the partial catalog this pass collected, so it is abandoned. The
	// error is deliberately not a TerminalError: the next reconcile is worth running.
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

// carryKnown reuses a recorded verdict. It is reached only for a tag NeedsExamination
// answers no for, which is never one recorded as removed from the repository: a tag the
// listing offers again has content behind it that the recorded verdict was not reached
// about, so it is examined rather than carried, and the removed marker is dropped by the
// fresh verdict that replaces it.
func carryKnown(version *semver.Version, known KnownVersion) *ChartVersion {
	return &ChartVersion{
		Version:            version,
		MediaType:          known.MediaType,
		UnavailableReason:  known.UnavailableReason,
		UnavailableMessage: known.UnavailableMessage,
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
