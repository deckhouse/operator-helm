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

package remove_helmapplication_rolebindings

import (
	"context"
	"fmt"

	"github.com/pkg/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/deckhouse/module-sdk/pkg"
	objectpatch "github.com/deckhouse/module-sdk/pkg/object-patch"
	"github.com/deckhouse/module-sdk/pkg/registry"
	"github.com/deckhouse/module-sdk/pkg/utils/ptr"

	"hooks/pkg/kube"
	"hooks/pkg/settings"
)

const (
	roleBindingAPIVersion = "rbac.authorization.k8s.io/v1"
	roleBindingKind       = "RoleBinding"

	roleBindingSnapshot = "applicationRoleBindingSnapshot"
)

// The labels the controller puts on every role binding it owns; they repeat
// v1alpha1.Label*, which this module cannot import.
var ownedRoleBindingLabels = map[string]string{
	"helm.deckhouse.io/managed-by": "operator-helm",
	"heritage":                     "deckhouse",
}

// resource addresses one role binding in its namespace.
type resource struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
}

var _ = registry.RegisterFunc(&pkg.HookConfig{
	OnAfterDeleteHelm: &pkg.OrderedConfig{Order: 15},
	AllowFailure:      false,
	Kubernetes: []pkg.KubernetesConfig{
		{
			Name:                         roleBindingSnapshot,
			APIVersion:                   roleBindingAPIVersion,
			Kind:                         roleBindingKind,
			LabelSelector:                &metav1.LabelSelector{MatchLabels: ownedRoleBindingLabels},
			JqFilter:                     `{name: .metadata.name, namespace: .metadata.namespace}`,
			ExecuteHookOnEvents:          ptr.To(false),
			ExecuteHookOnSynchronization: ptr.To(false),
		},
	},
}, removeRoleBindings)

func removeRoleBindings(ctx context.Context, input *pkg.HookInput) error {
	client, err := input.DC.GetK8sClient()
	if err != nil {
		return fmt.Errorf("getting k8s client: %w", err)
	}

	hasWorkloads, err := kube.NamespaceHasWorkloads(ctx, client.Dynamic(), settings.ModuleNamespace)
	if err != nil {
		return fmt.Errorf("checking workloads in namespace %s: %w", settings.ModuleNamespace, err)
	}

	// A live controller recreates the binding it owns, so nothing is deleted until it is gone.
	if hasWorkloads {
		return fmt.Errorf("namespace %s still has running deployments or pods, retrying before removing role bindings", settings.ModuleNamespace)
	}

	resources, err := objectpatch.UnmarshalToStruct[resource](input.Snapshots, roleBindingSnapshot)
	if err != nil {
		return errors.Wrap(err, "failed to get "+roleBindingSnapshot+" snapshot resources")
	}

	for _, res := range resources {
		input.PatchCollector.Delete(roleBindingAPIVersion, roleBindingKind, res.Namespace, res.Name)
	}

	return nil
}
