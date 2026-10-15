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

// Variables rather than constants so the tests that pin them need not wait or stream for real.
var (
	indexAttemptTimeout = 15 * time.Second // Bounds one attempt: connect, respond and hand over the whole index.
	indexFetchBudget    = time.Minute      // Caps the three attempts together with the pauses between them.
	maxIndexSize        = 64 << 20         // Bounds the index.yaml held in memory.
)

var errIndexTooLarge = errors.New("index.yaml is too large")

type HelmRepositoryIndex struct {
	// The yaml tag is spelled out because a YAML decoder ignores json tags and matches a
	// field by its lower-cased name, which would leave APIVersion empty.
	APIVersion string                                  `json:"apiVersion" yaml:"apiVersion"`
	Entries    map[string][]HelmRepositoryChartVersion `json:"entries"    yaml:"entries"`
}

type HelmRepositoryChartVersion struct {
	Icon    string   `json:"icon,omitempty"`
	Version string   `json:"version"`
	Digest  string   `json:"digest"`
	Removed bool     `json:"removed,omitempty"`
	URLs    []string `json:"urls,omitempty"`
}

// FetchCharts reads the repository index. FetchOptions is ignored: the index is a single
// document that already carries every version.
func (c *helmRepositoryClient) FetchCharts(ctx context.Context, url string, config *RepoConfig, _ FetchOptions) ([]Chart, error) {
	if !strings.HasSuffix(url, "/index.yaml") {
		url += "/index.yaml"
	}

	var indexFile HelmRepositoryIndex

	httpClient := http.DefaultClient
	if config != nil && (config.CACertificate != "" || config.Insecure) {
		tlsTransport, err := BuildTLSTransport(config)
		if err != nil {
			return nil, err
		}
		defer tlsTransport.CloseIdleConnections()

		httpClient = &http.Client{Transport: tlsTransport}
	}

	backoff := wait.Backoff{
		Duration: 1 * time.Second,
		Factor:   2.0,
		Jitter:   0.1,
		Steps:    3,
	}

	ctx, cancel := context.WithTimeout(ctx, indexFetchBudget)
	defer cancel()

	// lastErr keeps the cause: the backoff helper reports only its own timeout once the steps run out.
	var lastErr error

	err := wait.ExponentialBackoffWithContext(ctx, backoff, func(ctx context.Context) (done bool, err error) {
		// Each attempt gets its own deadline, carved out of the overall budget.
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

		// The body sent with a 429 decodes into an empty index, which would prune the whole catalog.
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

			// A body cut short is the same class of failure as a refused connection, so it is retried.
			lastErr = fmt.Errorf("reading the index of %s: %w", url, err)

			return false, nil
		}

		// A fresh value: a retry must not merge its entries into what a previous attempt parsed.
		var decoded HelmRepositoryIndex
		if err := yaml.Unmarshal(body, &decoded); err != nil {
			return true, fmt.Errorf("cannot decode response: %w", err)
		}

		// A document with neither member is not an index at all, and would read as a repository gone empty.
		if decoded.APIVersion == "" && decoded.Entries == nil {
			lastErr = fmt.Errorf("repository %s returned no index document (%d bytes, HTTP %d)", url, len(body), resp.StatusCode)

			return false, nil
		}

		indexFile = decoded

		return true, nil
	})
	if err != nil {
		if lastErr != nil && wait.Interrupted(err) {
			// Every attempt failed retriably, so err is the bare timeout; report the cause instead.
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
				// A single malformed entry must not cost the whole catalog.
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

func readIndexBody(body io.Reader, url string) ([]byte, error) {
	// One extra byte, so hitting the limit is distinguishable from ending exactly at it.
	data, err := io.ReadAll(io.LimitReader(body, int64(maxIndexSize)+1))
	if err != nil {
		return nil, err
	}

	if len(data) > maxIndexSize {
		return nil, fmt.Errorf("%w: repository %s sent more than %d MiB", errIndexTooLarge, url, maxIndexSize>>20)
	}

	return data, nil
}

func classifyChartLocation(version *ChartVersion, urls []string) {
	// Only urls[0] matters: that is the one the internal HelmChart downloads.
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
