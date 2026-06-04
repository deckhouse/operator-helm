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
	"strings"
	"time"

	"github.com/Masterminds/semver/v3"
	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

var OCIRepositoryDefaultClient ClientInterface = &ociRepositoryClient{}

type ociRepositoryClient struct{}

func (c *ociRepositoryClient) FetchCharts(ctx context.Context, url string, config *RepoConfig) ([]Chart, error) {
	url = trimSchemaPrefixes(url)
	url = strings.TrimSuffix(url, "/")

	if !strings.Contains(url, "/") {
		return nil, errors.New("url must contain chart/image name")
	}

	urlParts := strings.Split(url, "/")
	chartName := urlParts[len(urlParts)-1]

	if len(chartName) == 0 {
		return nil, errors.New("failed to parse chart/image name from the url")
	}

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
		return nil, fmt.Errorf("listing image tags: %w", err)
	}

	var chartVersions []ChartVersion

	for _, tag := range tags {
		if isCosignTag(tag) {
			continue
		}

		semVersion, err := semver.NewVersion(tag)
		if err != nil {
			continue
		}

		chartVersions = append(chartVersions, ChartVersion{Version: semVersion})
	}

	return []Chart{
		{
			Name:     chartName,
			Versions: chartVersions,
		},
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
