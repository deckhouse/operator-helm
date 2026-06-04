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
	"github.com/deckhouse/operator-helm/internal/utils"
)

type Chart struct {
	Name     string
	Versions []ChartVersion
}

type ChartVersion struct {
	Version *semver.Version
	IconURL string
}

type ClientInterface interface {
	FetchCharts(ctx context.Context, url string, config *RepoConfig) ([]Chart, error)
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
