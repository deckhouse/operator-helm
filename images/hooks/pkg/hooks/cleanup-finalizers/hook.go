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

// publicAPIVersion carries the module's own kinds. They are guarded by the
// helm.deckhouse.io/cleanup finalizer, which only our controller ever removes; the
// internal flux objects listed below instead carry flux's own finalizer, which only
// the flux controllers ever remove. Once the module is gone neither of those is
// running, so nothing is left to take either finalizer off and the hook has to.
const publicAPIVersion = "helm.deckhouse.io/v1alpha1"

const (
	// publicFinalizer is v1alpha1.FinalizerName; the hooks module does not depend on
	// the api module, so the string is repeated here.
	publicFinalizer = "helm.deckhouse.io/cleanup"
	// internalFinalizer is flux's own finalizers.fluxcd.io under the name it is
	// actually stored with: the kube-api-rewriter sidecar renames finalizers into the
	// internal domain on the way to the API server (see the Finalizers rule in
	// images/kube-api-rewriter/pkg/operatornelm/operatornelm_rules.go). This hook talks
	// to the API server directly, without the sidecar, so it must use the stored name.
	internalFinalizer = "finalizers.internal.operator-helm.deckhouse.io"
)

// watchedKind is one kind the hook strips a finalizer from. A kind is either
// confined to the module namespace (the internal flux objects, which are always
// created there) or unconfined: cluster-scoped kinds have no namespace at all and
// the namespaced public kinds live in arbitrary user namespaces.
type watchedKind struct {
	snapshot   string
	apiVersion string
	kind       string
	// finalizer is the one finalizer this module is responsible for on the kind.
	// Everything else on the object belongs to somebody else and stays.
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

	// Only the module's own finalizer may go: the public kinds live in user namespaces
	// and may well carry a backup or policy operator's finalizer, and our Deployments
	// being stopped says nothing about whether that operator is done. A merge patch
	// (RFC 7396) cannot express "drop one element", it can only null the whole member,
	// and a JSON patch "remove" of an index needs one — racy — and fails outright on an
	// object that has no finalizers left, while WithIgnoreMissingObject forgives only a
	// missing object, not a missing path. The jq filter is read-modify-write under an
	// optimistic lock: shell-operator gets the object, applies the filter, skips the
	// write when nothing changed and retries the whole cycle on conflict. That keeps the
	// operation idempotent — the hook may well run twice, a teardown can be retried after
	// a transient failure, and the first pass leaves objects it patched but could not
	// observe being deleted — and safe against a concurrent edit of the same list.
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

// removeFinalizerFilter builds the jq program dropping one finalizer. The guard keeps
// an object that never had finalizers untouched: mapping over an absent member is an
// error in jq, and writing an empty list back would be a pointless update.
func removeFinalizerFilter(finalizer string) string {
	return fmt.Sprintf(`if .metadata.finalizers then .metadata.finalizers -= [%q] else . end`, finalizer)
}
