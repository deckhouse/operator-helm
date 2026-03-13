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

package client

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
	"k8s.io/apimachinery/pkg/util/wait"

	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
)

var HelmRepositoryDefaultClient Interface = &helmRepositoryClient{}

type helmRepositoryClient struct{}

func (c *helmRepositoryClient) FetchCharts(ctx context.Context, url string, auth *AuthConfig) (map[string][]helmv1alpha1.HelmClusterAddonChartVersion, error) {
	if !strings.HasSuffix(url, "/index.yaml") {
		url += "/index.yaml"
	}

	var indexFile HelmRepositoryIndex

	backoff := wait.Backoff{
		Duration: 1 * time.Second, // Initial delay
		Factor:   2.0,             // Double the delay each time
		Jitter:   0.1,             // Add 10% randomness to prevent the thundering herd problem
		Steps:    3,               // Maximum number of retries (1s, 2s, 4s, 8s, 16s)
	}

	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()

	err := wait.ExponentialBackoffWithContext(ctx, backoff, func(ctx context.Context) (done bool, err error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return true, fmt.Errorf("creating request: %w", err)
		}

		if auth != nil {
			req.SetBasicAuth(auth.Username, auth.Password)
		}

		resp, err := http.DefaultClient.Do(req)
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

	charts := make(map[string][]helmv1alpha1.HelmClusterAddonChartVersion)

	for chartName, chartInfo := range indexFile.Entries {
		charts[chartName] = make([]helmv1alpha1.HelmClusterAddonChartVersion, 0)

		for _, chartVersion := range chartInfo {
			if chartVersion.Removed {
				continue
			}

			charts[chartName] = append(charts[chartName], helmv1alpha1.HelmClusterAddonChartVersion{
				Version: chartVersion.Version,
				Digest:  chartVersion.Digest,
			})
		}
	}

	return charts, nil
}

type HelmRepositoryIndex struct {
	APIVersion string                                  `json:"apiVersion"`
	Entries    map[string][]HelmRepositoryChartVersion `json:"entries"`
}

type HelmRepositoryChartVersion struct {
	Version string `json:"version"`
	Digest  string `json:"digest"`
	Removed bool   `json:"removed,omitempty"`
}
