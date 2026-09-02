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
	"net/http"

	"github.com/Masterminds/semver/v3"

	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
	"github.com/deckhouse/operator-helm/internal/utils"
)

type Chart struct {
	Name     string
	Versions []ChartVersion
}

type ChartVersion struct {
	Version *semver.Version
	IconURL string

	// MediaType is the OCI media type of the layer holding this chart version. It is
	// empty for helm repositories and for OCI versions that are not usable.
	MediaType string
	// UnavailableReason and UnavailableMessage are set when the version cannot be
	// deployed and are empty for a usable one.
	UnavailableReason  string
	UnavailableMessage string
}

// KnownVersion is the verdict a previous pass reached for one tag.
type KnownVersion struct {
	MediaType         string
	UnavailableReason string
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

// NeedsExamination reports whether a listed tag has to be examined again. A recorded
// media type is authoritative; an unsupported artifact is a verdict and is not
// re-examined until a full pass; anything else — never seen, seen without a verdict
// (an entry written before media types were recorded), or left pending — is examined.
func (o FetchOptions) NeedsExamination(chartName, tag string) bool {
	if o.Full {
		return true
	}

	known, recorded := o.Known[chartName][tag]
	switch {
	case !recorded:
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

func NewClient(repoType utils.InternalRepositoryType) (ClientInterface, error) {
	switch repoType {
	case utils.InternalHelmRepository:
		return HelmRepositoryDefaultClient, nil
	case utils.InternalOCIRepository:
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

func BuildTLSTransport(config *RepoConfig) *http.Transport {
	tlsConfig := &tls.Config{}

	if config.Insecure {
		tlsConfig.InsecureSkipVerify = true
	}

	if config.CACertificate != "" {
		caCertPool := x509.NewCertPool()
		caCertPool.AppendCertsFromPEM([]byte(config.CACertificate))
		tlsConfig.RootCAs = caCertPool
	}

	return &http.Transport{TLSClientConfig: tlsConfig}
}
