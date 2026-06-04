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
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/Masterminds/semver/v3"
)

var HelmRepositoryDefaultClient ClientInterface = &helmRepositoryClient{}

type helmRepositoryClient struct{}

type HelmRepositoryIndex struct {
	APIVersion string                                  `json:"apiVersion"`
	Entries    map[string][]HelmRepositoryChartVersion `json:"entries"`
}

type HelmRepositoryChartVersion struct {
	Icon    string `json:"icon,omitempty"`
	Version string `json:"version"`
	Digest  string `json:"digest"`
	Removed bool   `json:"removed,omitempty"`
}

func (c *helmRepositoryClient) FetchCharts(ctx context.Context, url string, config *RepoConfig) ([]Chart, error) {
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

	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()

	err := wait.ExponentialBackoffWithContext(ctx, backoff, func(ctx context.Context) (done bool, err error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return true, fmt.Errorf("creating request: %w", err)
		}

		if config != nil && config.Username != "" {
			req.SetBasicAuth(config.Username, config.Password)
		}

		resp, err := httpClient.Do(req)
		if err != nil {
			return false, nil
		}
		defer resp.Body.Close()

		if resp.StatusCode >= 500 {
			return false, nil
		}

		if resp.StatusCode >= 400 {
			return true, fmt.Errorf("fatal client error: received status %d", resp.StatusCode)
		}

		if err := yaml.NewDecoder(resp.Body).Decode(&indexFile); err != nil {
			return true, fmt.Errorf("cannot decode response: %w", err)
		}

		return true, nil
	})
	if err != nil {
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
				return nil, fmt.Errorf("failed to parse chart %q version %q: %w", chartName, chartVersion.Version, err)
			}

			chart.Versions = append(chart.Versions, ChartVersion{Version: semVersion, IconURL: chartVersion.Icon})
		}

		sort.Slice(chart.Versions, func(i, j int) bool {
			return chart.Versions[i].Version.GreaterThan(chart.Versions[j].Version)
		})

		charts = append(charts, chart)
	}

	return charts, nil
}
