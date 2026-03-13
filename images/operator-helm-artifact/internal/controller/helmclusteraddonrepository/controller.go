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

package helmclusteraddonrepository

import (
	sourcev1 "github.com/werf/nelm-source-controller/api/v1"
	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
	"github.com/deckhouse/operator-helm/internal/services"
	"github.com/deckhouse/operator-helm/internal/utils"
)

func SetupWithManager(mgr ctrl.Manager) error {
	client := mgr.GetClient()

	r := &reconciler{
		Client:            client,
		repositoryService: services.NewRepoService(client, mgr.GetScheme(), TargetNamespace),
		chartSyncService:  services.NewChartSyncService(client, mgr.GetScheme()),
		statusManager:     services.NewStatusManager(client, LabelManagedByValue),
	}

	return ctrl.NewControllerManagedBy(mgr).
		Named(ControllerName).
		For(&helmv1alpha1.HelmClusterAddonRepository{}).
		Watches(
			&sourcev1.HelmRepository{},
			handler.EnqueueRequestsFromMapFunc(utils.MapInternalToFacade(TargetNamespace, LabelManagedBy, LabelManagedByValue, LabelSourceName)),
			builder.WithPredicates(predicate.ResourceVersionChangedPredicate{}),
		).
		Watches(
			&corev1.Secret{},
			handler.EnqueueRequestsFromMapFunc(utils.MapInternalToFacade(TargetNamespace, LabelManagedBy, LabelManagedByValue, LabelSourceName)),
			builder.WithPredicates(predicate.ResourceVersionChangedPredicate{}),
		).
		Watches(
			&helmv1alpha1.HelmClusterAddonChart{},
			handler.EnqueueRequestForOwner(
				mgr.GetScheme(),
				mgr.GetRESTMapper(),
				&helmv1alpha1.HelmClusterAddonRepository{},
				handler.OnlyControllerOwner(),
			),
		).Complete(r)
}
