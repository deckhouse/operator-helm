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

package v1alpha1

import (
	"fmt"
	"strings"
)

// The media types below are the value domain of HelmClusterAddonChartVersion.MediaType
// and the rule for recognizing a packaged Helm chart inside an OCI artifact. They live
// in the API module because more than one component has to agree on them: the operator
// records a verdict against them, and the chart-values service examines the same
// artifacts to read values.yaml. Two copies of these lists would be two definitions of
// what counts as a chart.

// ChartConfigMediaTypes identify an OCI artifact as a packaged Helm chart. The layer
// media type alone cannot: application/tar+gzip is generic and any tarball may carry
// it, so the config is the authoritative marker.
var ChartConfigMediaTypes = []string{
	"application/vnd.cncf.helm.config.v1+json",
}

// ChartLayerMediaTypes hold a packaged chart, in priority order. The first entry
// present in a manifest wins regardless of the order of layers inside it, so an
// artifact carrying two supported layers resolves deterministically.
var ChartLayerMediaTypes = []string{
	"application/vnd.cncf.helm.chart.content.v1.tar+gzip",
	"application/tar+gzip",
}

// IsChartConfigMediaType reports whether an artifact's config marks it as a chart.
func IsChartConfigMediaType(mediaType string) bool {
	for _, supported := range ChartConfigMediaTypes {
		if mediaType == supported {
			return true
		}
	}

	return false
}

// SplitOCIRef splits the value of HelmClusterAddonChartVersion.OCIRef into the
// repository address and the tag. fallbackTag is used when the reference carries no
// tag of its own, which is how an index entry that relies on its own version field
// spells the reference; a reference read back from the catalog always carries one, so
// callers reading recorded data can pass an empty fallback.
//
// The returned address deliberately keeps the spelling it was given instead of being
// rebuilt from a parsed reference: registry libraries normalize some hosts (docker.io
// becomes index.docker.io), and the internal source object must address the registry
// the repository actually named.
//
// This is decomposition only, not validation: whether the reference is addressable at
// all is decided once, where it is first read from a repository index, and recorded as
// a verdict on the version.
func SplitOCIRef(ref, fallbackTag string) (string, string, error) {
	trimmed := strings.TrimPrefix(ref, "oci://")

	if strings.Contains(trimmed, "@") {
		return "", "", fmt.Errorf("oci reference %q addresses a digest, which cannot be expressed as a chart version tag", ref)
	}

	slash := strings.LastIndex(trimmed, "/")
	if slash < 0 {
		return "", "", fmt.Errorf("oci reference %q carries no chart path", ref)
	}

	repository, tag := trimmed, fallbackTag

	// The colon is looked for after the last slash only: a registry port lives before
	// it and is not a tag.
	if colon := strings.LastIndex(trimmed[slash+1:], ":"); colon >= 0 {
		repository = trimmed[:slash+1+colon]
		tag = trimmed[slash+1+colon+1:]
	}

	if tag == "" {
		return "", "", fmt.Errorf("oci reference %q carries no tag and no version to use instead", ref)
	}

	return "oci://" + repository, tag, nil
}
