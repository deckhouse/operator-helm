/*
Copyright 2026 Flant JSC

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

package cleanup_finalizers

import (
	"context"
	"hooks/pkg/settings"

	"github.com/deckhouse/module-sdk/pkg"
	objectpatch "github.com/deckhouse/module-sdk/pkg/object-patch"
	"github.com/deckhouse/module-sdk/pkg/registry"
	"github.com/deckhouse/module-sdk/pkg/utils/ptr"
	"github.com/pkg/errors"
)

const (
	internalHelmReleaseSnapshotName    = "internalHelmReleaseSnapshot"
	internalHelmChartSnapshotName      = "internalHelmChartSnapshot"
	internalHelmRepositorySnapshotName = "internalHelmRepositorySnapshot"
	internalOCIRepositorySnapshotName  = "internalOCIRepositorySnapshot"
)

type resource struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Name       string `json:"name"`
}

var _ = registry.RegisterFunc(&pkg.HookConfig{
	OnAfterDeleteHelm: &pkg.OrderedConfig{Order: 10},
	AllowFailure:      true,
	Kubernetes: []pkg.KubernetesConfig{
		{
			Name:       internalHelmReleaseSnapshotName,
			APIVersion: "helm.internal.operator-helm.deckhouse.io/v2",
			Kind:       "InternalNelmOperatorHelmRelease",
			JqFilter:   `{apiVersion: .apiVersion, kind: .kind, name: .metadata.name}`,
			NamespaceSelector: &pkg.NamespaceSelector{
				NameSelector: &pkg.NameSelector{
					MatchNames: []string{settings.ModuleNamespace},
				},
			},
			ExecuteHookOnEvents:          ptr.To(false),
			ExecuteHookOnSynchronization: ptr.To(false),
		},
		{
			Name:       internalHelmChartSnapshotName,
			APIVersion: "source.internal.operator-helm.deckhouse.io/v1",
			Kind:       "InternalNelmOperatorHelmChart",
			JqFilter:   `{apiVersion: .apiVersion, kind: .kind, name: .metadata.name}`,
			NamespaceSelector: &pkg.NamespaceSelector{
				NameSelector: &pkg.NameSelector{
					MatchNames: []string{settings.ModuleNamespace},
				},
			},
			ExecuteHookOnEvents:          ptr.To(false),
			ExecuteHookOnSynchronization: ptr.To(false),
		},
		{
			Name:       internalHelmRepositorySnapshotName,
			APIVersion: "source.internal.operator-helm.deckhouse.io/v1",
			Kind:       "InternalNelmOperatorHelmRepository",
			JqFilter:   `{apiVersion: .apiVersion, kind: .kind, name: .metadata.name}`,
			NamespaceSelector: &pkg.NamespaceSelector{
				NameSelector: &pkg.NameSelector{
					MatchNames: []string{settings.ModuleNamespace},
				},
			},
			ExecuteHookOnEvents:          ptr.To(false),
			ExecuteHookOnSynchronization: ptr.To(false),
		},
		{
			Name:       internalOCIRepositorySnapshotName,
			APIVersion: "source.internal.operator-helm.deckhouse.io/v1",
			Kind:       "InternalNelmOperatorOCIRepository",
			JqFilter:   `{apiVersion: .apiVersion, kind: .kind, name: .metadata.name}`,
			NamespaceSelector: &pkg.NamespaceSelector{
				NameSelector: &pkg.NameSelector{
					MatchNames: []string{settings.ModuleNamespace},
				},
			},
			ExecuteHookOnEvents:          ptr.To(false),
			ExecuteHookOnSynchronization: ptr.To(false),
		},
	},
}, cleanupFinalizer)

func cleanupFinalizer(ctx context.Context, input *pkg.HookInput) error {
	removeFinalizers := func(apiVersion, kind, name string) {
		input.PatchCollector.PatchWithJSON(
			[]map[string]any{
				{"op": "remove", "path": "/metadata/finalizers", "value": nil},
			},
			apiVersion,
			kind,
			settings.ModuleNamespace,
			name,
			objectpatch.WithIgnoreMissingObject(true),
		)
	}

	snapshots := []string{
		internalHelmReleaseSnapshotName,
		internalHelmChartSnapshotName,
		internalHelmRepositorySnapshotName,
		internalOCIRepositorySnapshotName,
	}

	for _, snapshot := range snapshots {
		resources, err := objectpatch.UnmarshalToStruct[resource](input.Snapshots, snapshot)
		if err != nil {
			return errors.Wrap(err, "failed to get "+snapshot+" snapshot resources")
		}

		for _, res := range resources {
			removeFinalizers(res.APIVersion, res.Kind, res.Name)
		}
	}

	return nil
}
