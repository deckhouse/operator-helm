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
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/Masterminds/semver/v3"
	"go.yaml.in/yaml/v3"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/log"

	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
	"github.com/deckhouse/operator-helm/internal/utils"
)

var HelmRepositoryDefaultClient ClientInterface = &helmRepositoryClient{}

type helmRepositoryClient struct{}

// The limits below are variables rather than constants for one reason: the tests
// that pin this behaviour would otherwise have to wait out real minutes or stream
// real megabytes. Nothing outside this package writes them.
var (
	// indexAttemptTimeout bounds one attempt — connect, respond and hand over the
	// whole index — on its own. A shared deadline for all attempts is what made the
	// retries decorative: against a slow mirror the first attempt drank the entire
	// budget and the backoff had nothing left to retry with. Fifteen seconds is a
	// GET of a single file with half of it left to the dial, and it matters because
	// a repository controller runs two reconciles at a time: a remote that accepts
	// the connection and then says nothing holds a worker for the whole attempt,
	// and the repositories behind it wait.
	indexAttemptTimeout = 15 * time.Second
	// indexFetchBudget caps the three attempts together with the pauses between
	// them (3*15s + ~1s + ~2s of backoff, jitter included). It is a read that runs
	// once per SyncInterval (5 minutes, see reconcile/repository), so spending up
	// to a minute on a struggling remote still leaves the next pass on schedule
	// and keeps one repository from occupying a worker indefinitely.
	indexFetchBudget = time.Minute
	// maxIndexSize bounds the index.yaml held in memory. Large public repositories
	// publish indexes of a few tens of megabytes (bitnami's is the extreme), so the
	// limit is set well above them while keeping a repository that streams without
	// end — or one that simply outgrew what this controller can mirror — from
	// inflating the controller's heap.
	maxIndexSize = 64 << 20
)

// errIndexTooLarge marks the size limit being hit, so the read is reported as
// oversize instead of being retried or, worse, decoded from a truncated document
// that YAML would happily accept as a smaller repository.
var errIndexTooLarge = errors.New("index.yaml is too large")

type HelmRepositoryIndex struct {
	APIVersion string                                  `json:"apiVersion"`
	Entries    map[string][]HelmRepositoryChartVersion `json:"entries"`
}

type HelmRepositoryChartVersion struct {
	Icon    string   `json:"icon,omitempty"`
	Version string   `json:"version"`
	Digest  string   `json:"digest"`
	Removed bool     `json:"removed,omitempty"`
	URLs    []string `json:"urls,omitempty"`
}

// FetchCharts reads the repository index. FetchOptions is ignored: the index is a
// single document that already carries every version, and the one thing it does not
// carry — the media type of a version published in a registry — is resolved lazily at
// deploy time rather than incrementally here.
func (c *helmRepositoryClient) FetchCharts(ctx context.Context, url string, config *RepoConfig, _ FetchOptions) ([]Chart, error) {
	if !strings.HasSuffix(url, "/index.yaml") {
		url += "/index.yaml"
	}

	var indexFile HelmRepositoryIndex

	httpClient := http.DefaultClient
	if config != nil && (config.CACertificate != "" || config.Insecure) {
		httpClient = &http.Client{Transport: BuildTLSTransport(config)}
	}

	backoff := wait.Backoff{
		Duration: 1 * time.Second,
		Factor:   2.0,
		Jitter:   0.1,
		Steps:    3,
	}

	ctx, cancel := context.WithTimeout(ctx, indexFetchBudget)
	defer cancel()

	// lastErr keeps the cause of the most recent retriable failure. The backoff
	// helper reports only its own timeout once the steps are exhausted, and a
	// transient read failure — 5xx, DNS, connection refused, TLS — is the most
	// common one there is: without this the operator would surface "timed out
	// waiting for the condition" as the whole diagnostic.
	var lastErr error

	err := wait.ExponentialBackoffWithContext(ctx, backoff, func(ctx context.Context) (done bool, err error) {
		// Each attempt gets its own deadline, carved out of the overall budget: the
		// point of retrying is that the next attempt may reach a healthier backend,
		// which requires it to still have time of its own.
		ctx, cancelAttempt := context.WithTimeout(ctx, indexAttemptTimeout)
		defer cancelAttempt()

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return true, fmt.Errorf("creating request: %w", err)
		}

		if config != nil && config.Username != "" {
			req.SetBasicAuth(config.Username, config.Password)
		}

		resp, err := httpClient.Do(req)
		if err != nil {
			lastErr = err

			return false, nil
		}
		defer resp.Body.Close()

		if resp.StatusCode >= 500 {
			lastErr = fmt.Errorf("repository %s is unavailable (HTTP %d)", url, resp.StatusCode)

			return false, nil
		}

		// A throttle is not terminal, and the nil that says so must not be read here
		// as permission to go on: the body a registry sends with it is JSON, which
		// decodes into an empty index without error, and the caller would then prune
		// the repository's whole catalog as if it had gone empty.
		if resp.StatusCode == http.StatusTooManyRequests {
			lastErr = fmt.Errorf("repository %s throttled the request (HTTP %d)", url, resp.StatusCode)

			return false, nil
		}

		if terminal := TerminalFromStatusCode(resp.StatusCode, url); terminal != nil {
			return true, terminal
		}

		body, err := readIndexBody(resp.Body, url)
		if err != nil {
			if errors.Is(err, errIndexTooLarge) {
				// Another attempt would download the same oversize document again.
				return true, err
			}

			// The body stopped mid-transfer — a reset connection or this attempt's
			// own deadline. That is the same class of failure as a refused
			// connection, so it is retried rather than reported as a broken index.
			lastErr = fmt.Errorf("reading the index of %s: %w", url, err)

			return false, nil
		}

		// Decoded into a fresh value: a retry must not merge its entries into
		// whatever a previous attempt managed to parse.
		var decoded HelmRepositoryIndex
		if err := yaml.Unmarshal(body, &decoded); err != nil {
			return true, fmt.Errorf("cannot decode response: %w", err)
		}

		indexFile = decoded

		return true, nil
	})
	if err != nil {
		if lastErr != nil && wait.Interrupted(err) {
			// The loop ran out of steps or the context ended with every attempt
			// failing retriably, so err is the bare timeout. Report the cause
			// instead. A terminal error never reaches here: it stops the loop as
			// the callback's own error and stays reachable through errors.As.
			return nil, fmt.Errorf("helm repository index.yaml request failed: %w", lastErr)
		}

		return nil, fmt.Errorf("helm repository index.yaml request failed: %w", err)
	}

	charts := make([]Chart, 0, len(indexFile.Entries))

	for chartName, chartInfo := range indexFile.Entries {
		chart := Chart{Name: chartName, Versions: make([]ChartVersion, 0, len(chartInfo))}

		for _, chartVersion := range chartInfo {
			if chartVersion.Removed {
				continue
			}

			semVersion, err := semver.NewVersion(chartVersion.Version)
			if err != nil {
				// A single malformed entry must not cost the whole catalog: the OCI
				// client already skips such tags, and the repository owner may publish
				// non-semver artifacts we simply cannot address.
				log.FromContext(ctx).V(1).Info("Skipping chart version that is not valid semver",
					"chart", chartName, "version", chartVersion.Version)

				continue
			}

			version := ChartVersion{Version: semVersion, IconURL: chartVersion.Icon}
			classifyChartLocation(&version, chartVersion.URLs)

			chart.Versions = append(chart.Versions, version)
		}

		sort.Slice(chart.Versions, func(i, j int) bool {
			return chart.Versions[i].Version.GreaterThan(chart.Versions[j].Version)
		})

		charts = append(charts, chart)
	}

	return charts, nil
}

// readIndexBody reads at most maxIndexSize bytes of the index. One extra byte is
// requested so that hitting the limit is distinguishable from a document that
// happens to end exactly at it.
func readIndexBody(body io.Reader, url string) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(body, int64(maxIndexSize)+1))
	if err != nil {
		return nil, err
	}

	if len(data) > maxIndexSize {
		return nil, fmt.Errorf("%w: repository %s sent more than %d MiB", errIndexTooLarge, url, maxIndexSize>>20)
	}

	return data, nil
}

// classifyChartLocation records where an index entry publishes its version. Only the
// first url is examined: the internal HelmChart resolves the version through the
// internal HelmRepository and downloads urls[0], so an entry whose first url is a
// registry reference cannot be served through the helm path at all, no matter what
// the remaining urls offer.
//
// An unusable reference is a verdict about the index entry: recording it as such is
// what keeps the addon from being sent down the helm path only to fail on the same
// url with an opaque error from the source controller.
func classifyChartLocation(version *ChartVersion, urls []string) {
	if len(urls) == 0 || !strings.HasPrefix(urls[0], "oci://") {
		return
	}

	url, tag, err := utils.SplitOCIRef(urls[0], version.Version.Original())
	if err != nil {
		version.UnavailableReason = helmv1alpha1.UnavailableReasonInvalidChartReference
		version.UnavailableMessage = truncate(err.Error())

		return
	}

	version.OCIRef = url + ":" + tag
}
