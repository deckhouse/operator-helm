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
	"fmt"
	"strings"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

// supportedChartConfigMediaTypes identify an OCI artifact as a packaged Helm chart.
// The layer media type alone cannot: application/tar+gzip is generic and any tarball
// may carry it, so the config is the authoritative marker.
var supportedChartConfigMediaTypes = []types.MediaType{
	"application/vnd.cncf.helm.config.v1+json",
}

// supportedChartLayerMediaTypes hold a packaged chart, in priority order. The first
// entry present in the manifest wins regardless of the order of layers inside it, so
// an artifact carrying two supported layers resolves deterministically.
var supportedChartLayerMediaTypes = []types.MediaType{
	"application/vnd.cncf.helm.chart.content.v1.tar+gzip",
	"application/tar+gzip",
}

// chartVerdict is the outcome of examining one tag. MediaType is set only for an
// artifact recognized as a chart; Message explains a negative verdict and is meant
// for the status of the chart version.
type chartVerdict struct {
	MediaType string
	Message   string
}

// OK reports whether the artifact is a usable chart.
func (v chartVerdict) OK() bool { return v.MediaType != "" }

// examineManifest decides whether a manifest describes a packaged Helm chart and, if
// it does, which layer media type holds it. descMediaType is the media type of the
// manifest descriptor: an index is rejected here rather than left to fail later,
// because an index is never a chart and a failure would be re-probed forever.
func examineManifest(descMediaType types.MediaType, manifest *v1.Manifest) chartVerdict {
	if descMediaType.IsIndex() {
		return chartVerdict{
			Message: fmt.Sprintf("the tag points to an index (%s), not to a chart manifest", descMediaType),
		}
	}

	if !isSupportedChartConfig(manifest.Config.MediaType) {
		return chartVerdict{
			Message: fmt.Sprintf("config media type %q is not a helm chart config", manifest.Config.MediaType),
		}
	}

	for _, supported := range supportedChartLayerMediaTypes {
		for _, layer := range manifest.Layers {
			if layer.MediaType == supported {
				return chartVerdict{MediaType: string(supported)}
			}
		}
	}

	return chartVerdict{
		Message: fmt.Sprintf("no supported chart layer, the artifact has [%s]", layerMediaTypes(manifest)),
	}
}

func isSupportedChartConfig(mediaType types.MediaType) bool {
	for _, supported := range supportedChartConfigMediaTypes {
		if mediaType == supported {
			return true
		}
	}

	return false
}

func layerMediaTypes(manifest *v1.Manifest) string {
	found := make([]string, 0, len(manifest.Layers))
	for _, layer := range manifest.Layers {
		found = append(found, string(layer.MediaType))
	}

	return strings.Join(found, ", ")
}
