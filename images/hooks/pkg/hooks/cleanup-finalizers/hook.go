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

const publicAPIVersion = "helm.deckhouse.io/v1alpha1"

const (
	// publicFinalizer repeats v1alpha1.FinalizerName, which this module cannot import.
	publicFinalizer = "helm.deckhouse.io/cleanup"
	// internalFinalizer is flux's finalizers.fluxcd.io under the name kube-api-rewriter stores it as.
	internalFinalizer = "finalizers.internal.operator-helm.deckhouse.io"
)

// watchedKind is one kind the hook strips its own finalizer from.
type watchedKind struct {
	snapshot   string
	apiVersion string
	kind       string
	// finalizer is the one finalizer this module owns on the kind; any other one stays.
	finalizer string
	// moduleNamespaceOnly narrows the binding to settings.ModuleNamespace.
	moduleNamespaceOnly bool
}

var watchedKinds = []watchedKind{
	{
		snapshot:            "internalHelmReleaseSnapshot",
		apiVersion:          "helm.internal.operator-helm.deckhouse.io/v2",
		kind:                "InternalNelmOperatorHelmRelease",
		finalizer:           internalFinalizer,
		moduleNamespaceOnly: true,
	},
	{
		snapshot:            "internalHelmChartSnapshot",
		apiVersion:          "source.internal.operator-helm.deckhouse.io/v1",
		kind:                "InternalNelmOperatorHelmChart",
		finalizer:           internalFinalizer,
		moduleNamespaceOnly: true,
	},
	{
		snapshot:            "internalHelmRepositorySnapshot",
		apiVersion:          "source.internal.operator-helm.deckhouse.io/v1",
		kind:                "InternalNelmOperatorHelmRepository",
		finalizer:           internalFinalizer,
		moduleNamespaceOnly: true,
	},
	{
		snapshot:            "internalOCIRepositorySnapshot",
		apiVersion:          "source.internal.operator-helm.deckhouse.io/v1",
		kind:                "InternalNelmOperatorOCIRepository",
		finalizer:           internalFinalizer,
		moduleNamespaceOnly: true,
	},
	{snapshot: "helmApplicationSnapshot", apiVersion: publicAPIVersion, kind: "HelmApplication", finalizer: publicFinalizer},
	{snapshot: "helmApplicationRepositorySnapshot", apiVersion: publicAPIVersion, kind: "HelmApplicationRepository", finalizer: publicFinalizer},
	{snapshot: "helmClusterAddonSnapshot", apiVersion: publicAPIVersion, kind: "HelmClusterAddon", finalizer: publicFinalizer},
	{snapshot: "helmClusterAddonRepositorySnapshot", apiVersion: publicAPIVersion, kind: "HelmClusterAddonRepository", finalizer: publicFinalizer},
	{snapshot: "helmClusterApplicationRepositorySnapshot", apiVersion: publicAPIVersion, kind: "HelmClusterApplicationRepository", finalizer: publicFinalizer},
}

// resource addresses one object in a patch; Namespace is empty for cluster-scoped kinds.
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
			Name:                         watched.snapshot,
			APIVersion:                   watched.apiVersion,
			Kind:                         watched.kind,
			JqFilter:                     `{apiVersion: .apiVersion, kind: .kind, name: .metadata.name, namespace: .metadata.namespace}`,
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

	// A jq patch is read-modify-write under an optimistic lock, so a repeated run is a no-op.
	removeFinalizer := func(res resource, finalizer string) {
		input.PatchCollector.PatchWithJQ(
			removeFinalizerFilter(finalizer),
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
			removeFinalizer(res, watched.finalizer)
		}
	}

	return nil
}

// removeFinalizerFilter builds the jq program dropping one finalizer, or leaving the object as is.
func removeFinalizerFilter(finalizer string) string {
	return fmt.Sprintf(`if .metadata.finalizers then .metadata.finalizers -= [%q] else . end`, finalizer)
}
