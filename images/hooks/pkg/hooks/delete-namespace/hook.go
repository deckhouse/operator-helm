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

package delete_namespace

import (
	"context"
	"fmt"
	"hooks/pkg/kube"
	"hooks/pkg/settings"

	"github.com/deckhouse/module-sdk/pkg"
	"github.com/deckhouse/module-sdk/pkg/registry"
)

var _ = registry.RegisterFunc(&pkg.HookConfig{
	OnAfterDeleteHelm: &pkg.OrderedConfig{Order: 20},
	AllowFailure:      false,
}, deleteNamespace)

func deleteNamespace(ctx context.Context, input *pkg.HookInput) error {
	client, err := input.DC.GetK8sClient()
	if err != nil {
		return fmt.Errorf("getting k8s client: %w", err)
	}

	hasWorkloads, err := kube.NamespaceHasWorkloads(ctx, client.Dynamic(), settings.ModuleNamespace)
	if err != nil {
		return fmt.Errorf("checking workloads in namespace %s: %w", settings.ModuleNamespace, err)
	}

	if hasWorkloads {
		return fmt.Errorf("namespace %s still has running deployments or pods, retrying before deletion", settings.ModuleNamespace)
	}

	input.PatchCollector.Delete("v1", "Namespace", "", settings.ModuleNamespace)

	return nil
}
