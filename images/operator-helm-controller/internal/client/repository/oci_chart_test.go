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
	"strings"
	"testing"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

func manifestWith(configType types.MediaType, layerTypes ...types.MediaType) *v1.Manifest {
	manifest := &v1.Manifest{
		SchemaVersion: 2,
		MediaType:     types.OCIManifestSchema1,
		Config:        v1.Descriptor{MediaType: configType},
	}
	for _, layerType := range layerTypes {
		manifest.Layers = append(manifest.Layers, v1.Descriptor{MediaType: layerType})
	}

	return manifest
}

func TestExamineManifest(t *testing.T) {
	const (
		helmConfig  = types.MediaType("application/vnd.cncf.helm.config.v1+json")
		currentType = "application/vnd.cncf.helm.chart.content.v1.tar+gzip"
		legacyType  = "application/tar+gzip"
		provType    = types.MediaType("application/vnd.cncf.helm.chart.provenance.v1.prov")
	)

	cases := []struct {
		name          string
		descMediaType types.MediaType
		manifest      *v1.Manifest
		wantMediaType string
		wantMessage   string
	}{
		{
			name:          "current layer type",
			descMediaType: types.OCIManifestSchema1,
			manifest:      manifestWith(helmConfig, currentType),
			wantMediaType: currentType,
		},
		{
			name:          "legacy layer type",
			descMediaType: types.OCIManifestSchema1,
			manifest:      manifestWith(helmConfig, legacyType),
			wantMediaType: legacyType,
		},
		{
			name:          "chart layer is not the first one",
			descMediaType: types.OCIManifestSchema1,
			manifest:      manifestWith(helmConfig, provType, legacyType),
			wantMediaType: legacyType,
		},
		{
			name:          "the list order wins over the manifest order",
			descMediaType: types.OCIManifestSchema1,
			manifest:      manifestWith(helmConfig, legacyType, currentType),
			wantMediaType: currentType,
		},
		{
			name:          "an index is not a chart",
			descMediaType: types.OCIImageIndex,
			manifest:      manifestWith(helmConfig, currentType),
			wantMessage:   "index",
		},
		{
			name:          "a foreign config is not a chart",
			descMediaType: types.OCIManifestSchema1,
			manifest:      manifestWith("application/vnd.unknown.config.v1+json", legacyType),
			wantMessage:   "application/vnd.unknown.config.v1+json",
		},
		{
			name:          "no supported layer",
			descMediaType: types.OCIManifestSchema1,
			manifest:      manifestWith(helmConfig, provType),
			wantMessage:   string(provType),
		},
		{
			name:          "no layers at all",
			descMediaType: types.OCIManifestSchema1,
			manifest:      manifestWith(helmConfig),
			wantMessage:   "no supported chart layer",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			verdict := examineManifest(tc.descMediaType, tc.manifest)

			if tc.wantMediaType != "" {
				if !verdict.OK() {
					t.Fatalf("expected a chart verdict, got message %q", verdict.Message)
				}
				if verdict.MediaType != tc.wantMediaType {
					t.Fatalf("media type is %q, want %q", verdict.MediaType, tc.wantMediaType)
				}
				return
			}

			if verdict.OK() {
				t.Fatalf("expected a negative verdict, got media type %q", verdict.MediaType)
			}
			if !strings.Contains(verdict.Message, tc.wantMessage) {
				t.Fatalf("message %q does not mention %q", verdict.Message, tc.wantMessage)
			}
		})
	}
}
