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
	"fmt"

	"github.com/pkg/errors"

	"github.com/deckhouse/module-sdk/pkg"
	objectpatch "github.com/deckhouse/module-sdk/pkg/object-patch"
	"github.com/deckhouse/module-sdk/pkg/registry"
	"github.com/deckhouse/module-sdk/pkg/utils/ptr"

	"hooks/pkg/kube"
	"hooks/pkg/settings"
)

// publicAPIVersion carries the module's own kinds; every kind listed below is guarded
// by the helm.deckhouse.io/cleanup finalizer, which only our controller ever removes.
// Once the module is gone nothing is left to remove it, so the hook has to.
const publicAPIVersion = "helm.deckhouse.io/v1alpha1"

// watchedKind is one kind the hook strips finalizers from. A kind is either
// confined to the module namespace (the internal flux objects, which are always
// created there) or unconfined: cluster-scoped kinds have no namespace at all and
// the namespaced public kinds live in arbitrary user namespaces.
type watchedKind struct {
	snapshot   string
	apiVersion string
	kind       string
	// moduleNamespaceOnly narrows the binding to settings.ModuleNamespace.
	moduleNamespaceOnly bool
}

var watchedKinds = []watchedKind{
	{
		snapshot:            "internalHelmReleaseSnapshot",
		apiVersion:          "helm.internal.operator-helm.deckhouse.io/v2",
		kind:                "InternalNelmOperatorHelmRelease",
		moduleNamespaceOnly: true,
	},
	{
		snapshot:            "internalHelmChartSnapshot",
		apiVersion:          "source.internal.operator-helm.deckhouse.io/v1",
		kind:                "InternalNelmOperatorHelmChart",
		moduleNamespaceOnly: true,
	},
	{
		snapshot:            "internalHelmRepositorySnapshot",
		apiVersion:          "source.internal.operator-helm.deckhouse.io/v1",
		kind:                "InternalNelmOperatorHelmRepository",
		moduleNamespaceOnly: true,
	},
	{
		snapshot:            "internalOCIRepositorySnapshot",
		apiVersion:          "source.internal.operator-helm.deckhouse.io/v1",
		kind:                "InternalNelmOperatorOCIRepository",
		moduleNamespaceOnly: true,
	},
	{snapshot: "helmApplicationSnapshot", apiVersion: publicAPIVersion, kind: "HelmApplication"},
	{snapshot: "helmApplicationRepositorySnapshot", apiVersion: publicAPIVersion, kind: "HelmApplicationRepository"},
	{snapshot: "helmClusterAddonSnapshot", apiVersion: publicAPIVersion, kind: "HelmClusterAddon"},
	{snapshot: "helmClusterAddonRepositorySnapshot", apiVersion: publicAPIVersion, kind: "HelmClusterAddonRepository"},
	{snapshot: "helmClusterApplicationRepositorySnapshot", apiVersion: publicAPIVersion, kind: "HelmClusterApplicationRepository"},
}

// resource is the snapshot projection: enough to address the object in a patch.
// Namespace is empty for cluster-scoped kinds, which is exactly what the patch
// collector expects for them.
type resource struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	Namespace  string `json:"namespace"`
}

func kubernetesConfigs() []pkg.KubernetesConfig {
	configs := make([]pkg.KubernetesConfig, 0, len(watchedKinds))

	for _, watched := range watchedKinds {
		config := pkg.KubernetesConfig{
			Name:       watched.snapshot,
			APIVersion: watched.apiVersion,
			Kind:       watched.kind,
			JqFilter:   `{apiVersion: .apiVersion, kind: .kind, name: .metadata.name, namespace: .metadata.namespace}`,
			// The hook only ever runs on module delete, so tracking events is pointless.
			ExecuteHookOnEvents:          ptr.To(false),
			ExecuteHookOnSynchronization: ptr.To(false),
		}

		if watched.moduleNamespaceOnly {
			config.NamespaceSelector = &pkg.NamespaceSelector{
				NameSelector: &pkg.NameSelector{
					MatchNames: []string{settings.ModuleNamespace},
				},
			}
		}

		configs = append(configs, config)
	}

	return configs
}

var _ = registry.RegisterFunc(&pkg.HookConfig{
	OnAfterDeleteHelm: &pkg.OrderedConfig{Order: 10},
	AllowFailure:      false,
	Kubernetes:        kubernetesConfigs(),
}, cleanupFinalizer)

func cleanupFinalizer(ctx context.Context, input *pkg.HookInput) error {
	client, err := input.DC.GetK8sClient()
	if err != nil {
		return fmt.Errorf("getting k8s client: %w", err)
	}

	hasWorkloads, err := kube.NamespaceHasWorkloads(ctx, client.Dynamic(), settings.ModuleNamespace)
	if err != nil {
		return fmt.Errorf("checking workloads in namespace %s: %w", settings.ModuleNamespace, err)
	}

	if hasWorkloads {
		return fmt.Errorf("namespace %s still has running deployments or pods, retrying before removing finalizers", settings.ModuleNamespace)
	}

	// A merge patch (RFC 7396) that nulls the member removes every finalizer and stays
	// a no-op on an object that has none. A JSON patch "remove" of /metadata/finalizers
	// would instead fail on such an object, and WithIgnoreMissingObject only forgives a
	// missing object, not a missing path. The hook may well run twice — a teardown can be
	// retried after a transient failure, and the first pass leaves objects it patched but
	// could not observe being deleted — so it has to survive a repeat.
	removeFinalizers := func(res resource) {
		input.PatchCollector.PatchWithMerge(
			map[string]any{"metadata": map[string]any{"finalizers": nil}},
			res.APIVersion,
			res.Kind,
			res.Namespace,
			res.Name,
			objectpatch.WithIgnoreMissingObject(true),
		)
	}

	for _, watched := range watchedKinds {
		resources, err := objectpatch.UnmarshalToStruct[resource](input.Snapshots, watched.snapshot)
		if err != nil {
			return errors.Wrap(err, "failed to get "+watched.snapshot+" snapshot resources")
		}

		for _, res := range resources {
			removeFinalizers(res)
		}
	}

	return nil
}
