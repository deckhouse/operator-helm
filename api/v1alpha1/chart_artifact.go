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

// ChartConfigMediaTypes identify an OCI artifact as a packaged Helm chart.
var ChartConfigMediaTypes = []string{
	"application/vnd.cncf.helm.config.v1+json",
}

// ChartLayerMediaTypes hold a packaged chart, in priority order. The first entry
// present in a manifest wins regardless of the order of layers inside it.
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

// SplitOCIRef splits the value of ChartVersion.OCIRef into the repository address and
// the tag, using fallbackTag when the reference carries no tag of its own.
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

	// The colon is looked for after the last slash only: a registry port is not a tag.
	if colon := strings.LastIndex(trimmed[slash+1:], ":"); colon >= 0 {
		repository = trimmed[:slash+1+colon]
		tag = trimmed[slash+1+colon+1:]
	}

	if tag == "" {
		return "", "", fmt.Errorf("oci reference %q carries no tag and no version to use instead", ref)
	}

	return "oci://" + repository, tag, nil
}
