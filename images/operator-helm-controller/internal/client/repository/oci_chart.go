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

	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
)

type chartVerdict struct {
	MediaType string
	Message   string
}

// OK reports whether the artifact is a usable chart.
func (v chartVerdict) OK() bool { return v.MediaType != "" }

func examineManifest(descMediaType types.MediaType, manifest *v1.Manifest) chartVerdict {
	// An index is rejected here rather than left to fail later: it is never a chart.
	if descMediaType.IsIndex() {
		return chartVerdict{
			Message: fmt.Sprintf("the tag points to an index (%s), not to a chart manifest", descMediaType),
		}
	}

	if !helmv1alpha1.IsChartConfigMediaType(string(manifest.Config.MediaType)) {
		return chartVerdict{
			Message: fmt.Sprintf("config media type %q is not a helm chart config", manifest.Config.MediaType),
		}
	}

	for _, supported := range helmv1alpha1.ChartLayerMediaTypes {
		for _, layer := range manifest.Layers {
			if string(layer.MediaType) == supported {
				return chartVerdict{MediaType: supported}
			}
		}
	}

	return chartVerdict{
		Message: fmt.Sprintf("no supported chart layer, the artifact has [%s]", layerMediaTypes(manifest)),
	}
}

func layerMediaTypes(manifest *v1.Manifest) string {
	found := make([]string, 0, len(manifest.Layers))
	for _, layer := range manifest.Layers {
		found = append(found, string(layer.MediaType))
	}

	return strings.Join(found, ", ")
}
