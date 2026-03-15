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
	"strings"
	"time"

	"github.com/Masterminds/semver/v3"
	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
)

var OCIRepositoryDefaultClient ClientInterface = &ociRepositoryClient{}

type ociRepositoryClient struct{}

func (c *ociRepositoryClient) FetchCharts(ctx context.Context, url string, auth *AuthConfig) (map[string][]helmv1alpha1.HelmClusterAddonChartVersion, error) {
	url = trimSchemaPrefixes(url)
	urlParts := strings.Split(url, "/")
	chartName := urlParts[len(urlParts)-1]

	repo, err := name.NewRepository(url)
	if err != nil {
		return nil, fmt.Errorf("failed to parse repository url: %w", err)
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

	if auth != nil {
		options = append(options, remote.WithAuth(authn.FromConfig(authn.AuthConfig{
			Username: auth.Username,
			Password: auth.Password,
		})))
	}

	tags, err := remote.List(repo, options...)
	if err != nil {
		return nil, fmt.Errorf("listing image tags: %w", err)
	}

	var chartVersions []helmv1alpha1.HelmClusterAddonChartVersion

	for _, tag := range tags {
		if isCosignTag(tag) || !isSemverCompliantTag(tag) {
			continue
		}

		// Do not obtain digests as they are currently not used and require a HEAD request per tag.
		chartVersions = append(chartVersions, helmv1alpha1.HelmClusterAddonChartVersion{
			Version: tag,
		})
	}

	return map[string][]helmv1alpha1.HelmClusterAddonChartVersion{
		chartName: chartVersions,
	}, nil
}

func trimSchemaPrefixes(url string) string {
	for _, prefix := range []string{"oci://", "http://", "https://"} {
		url = strings.TrimPrefix(url, prefix)
	}

	return url
}

func isSemverCompliantTag(tag string) bool {
	_, err := semver.NewVersion(tag)
	return err == nil
}

func isCosignTag(tag string) bool {
	for _, suffix := range []string{".att", ".sbom", ".sig"} {
		if strings.HasSuffix(tag, suffix) {
			return true
		}
	}

	return false
}
