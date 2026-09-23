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

// Package chartsource names where one chart version comes from. A release is served
// either by an internal HelmChart, when the repository hands out packaged archives,
// or by an internal OCIRepository, when it hands out registry artifacts — and which
// of the two applies is a property of the version, not of the repository alone. The
// vocabulary lives in its own package because every layer speaks it: the repository
// client picks an implementation by Kind, the services write the internal object it
// names, and both reconcilers branch on it.
package chartsource

import (
	"fmt"
	"net/url"

	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
	"github.com/deckhouse/operator-helm/internal/utils"
)

// Kind is which internal source object serves a chart: an internal HelmRepository
// hands out packaged archives, an internal OCIRepository registry artifacts.
type Kind string

const (
	Helm Kind = "helm"
	OCI  Kind = "oci"
)

// Source is where one chart version is actually fetched from. It is not the
// same thing as the repository type: the repository type follows the scheme of
// spec.url and decides the catalog client, the shape of the auth secret and whether
// an internal HelmRepository exists at all, while Source decides which internal
// source object one addon needs for the version it asks for. The two differ exactly
// when a helm repository's index points a version at a registry.
type Source struct {
	Kind Kind
	// URL is the artifact address with the oci:// scheme and without the tag. It is
	// empty for Kind == Helm.
	URL string
	// Tag is the artifact tag. It is empty for Kind == Helm.
	Tag string
}

// Resolve decides where one chart version comes from. A recorded OCI reference wins
// over the repository scheme: that is the hybrid case this exists for. It takes the
// repository url rather than the repository object so that every repository kind can
// use it.
func Resolve(
	repoURL string,
	version *helmv1alpha1.ChartVersion,
) (Source, error) {
	if version.OCIRef != "" {
		// The recorded reference always carries a tag, so there is no fallback to
		// offer here; a reference that cannot be split was never recorded by the
		// catalog and can only come from data written by hand or by an older version.
		url, tag, err := utils.SplitOCIRef(version.OCIRef, "")
		if err != nil {
			return Source{}, fmt.Errorf("resolving the source of version %q: %w", version.Version, err)
		}

		return Source{Kind: OCI, URL: url, Tag: tag}, nil
	}

	repoType, err := KindOf(repoURL)
	if err != nil {
		return Source{}, fmt.Errorf("resolving the source of version %q: %w", version.Version, err)
	}

	if repoType == OCI {
		return Source{Kind: OCI, URL: repoURL, Tag: version.Version}, nil
	}

	return Source{Kind: Helm}, nil
}

// KindOf reads the kind off a repository url alone. It answers for the repository
// as a whole — which catalog client to use, which auth secret shape, whether an
// internal HelmRepository exists at all — where Resolve answers for one version.
func KindOf(s string) (Kind, error) {
	parsedURL, err := url.Parse(s)
	if err != nil {
		return "", fmt.Errorf("cannot parse url: %w", err)
	}

	switch parsedURL.Scheme {
	case "http", "https":
		return Helm, nil
	case "oci":
		return OCI, nil
	default:
		return "", fmt.Errorf("unsupported repository schema in use: %s", parsedURL.Scheme)
	}
}

// GetRegistryHost extracts the registry host (with port, if any) from a repository
// URL. Registry credentials in the docker config format are keyed by host, the
// chart path inside the registry is not part of the key.
