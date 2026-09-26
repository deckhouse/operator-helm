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

package helmclusterapplicationrepository

import (
	"sigs.k8s.io/controller-runtime/pkg/handler"

	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
	"github.com/deckhouse/operator-helm/internal/utils"
)

// mapInternalResources maps an internal object back to the repository it was
// derived from. Every family's internal objects share the operator namespace, so
// the source-name label of this kind is what tells them apart; the source is
// cluster-scoped, so its name alone identifies it.
func mapInternalResources() handler.MapFunc {
	return utils.MapInternalResources(
		ControllerName,
		helmv1alpha1.TargetNamespace,
		helmv1alpha1.LabelManagedBy,
		helmv1alpha1.LabelManagedByValue,
		helmv1alpha1.HelmClusterApplicationRepositoryLabelSourceName,
	)
}
