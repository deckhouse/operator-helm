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

package helmclusteraddon

import (
	helmv2 "github.com/werf/3p-helm-controller/api/v2"
	sourcev1 "github.com/werf/nelm-source-controller/api/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
	"github.com/deckhouse/operator-helm/internal/adapter"
	"github.com/deckhouse/operator-helm/internal/manager/status"
	reconcile "github.com/deckhouse/operator-helm/internal/reconcile/release"
	"github.com/deckhouse/operator-helm/internal/services"
	"github.com/deckhouse/operator-helm/internal/source"
	"github.com/deckhouse/operator-helm/internal/utils"
)

const (
	ControllerName = "helmclusteraddon-controller"
)

func SetupWithManager(mgr ctrl.Manager) error {
	client := mgr.GetClient()

	r := reconcile.New(client, reconcile.Deps{
		NewRelease:   adapter.EmptyAddonRelease,
		Repositories: adapter.NewAddonRepositoryResolver(client),
		Chart:        services.NewChartService(client, mgr.GetScheme(), helmv1alpha1.TargetNamespace),
		OCI:          services.NewOCIRepoService(client, mgr.GetScheme(), helmv1alpha1.TargetNamespace, nil),
		Release:      services.NewReleaseService(client, mgr.GetScheme(), helmv1alpha1.TargetNamespace),
		Maintenance:  services.NewMaintenanceService(client, mgr.GetScheme(), helmv1alpha1.TargetNamespace),
		Claim:        services.NewClaimService(client, mgr.GetAPIReader(), helmv1alpha1.TargetNamespace),
		Namespaces:   services.NewNamespaceService(client),
		Access:       source.NoAccess{},
		Status:       status.NewManager(client),
	})

	return ctrl.NewControllerManagedBy(mgr).
		Named(ControllerName).
		WithOptions(controller.Options{MaxConcurrentReconciles: 2}).
		For(
			&helmv1alpha1.HelmClusterAddon{},
			builder.WithPredicates(predicate.Or(
				predicate.GenerationChangedPredicate{},
				predicate.AnnotationChangedPredicate{},
			)),
		).
		Watches(
			&sourcev1.HelmChart{},
			handler.EnqueueRequestsFromMapFunc(
				utils.MapInternalResources(
					ControllerName,
					helmv1alpha1.TargetNamespace,
					helmv1alpha1.LabelManagedBy,
					helmv1alpha1.LabelManagedByValue,
					helmv1alpha1.HelmClusterAddonLabelSourceName,
				),
			),
			builder.WithPredicates(predicate.ResourceVersionChangedPredicate{}),
		).
		Watches(
			&helmv2.HelmRelease{},
			handler.EnqueueRequestsFromMapFunc(
				utils.MapInternalResources(
					ControllerName,
					helmv1alpha1.TargetNamespace,
					helmv1alpha1.LabelManagedBy,
					helmv1alpha1.LabelManagedByValue,
					helmv1alpha1.HelmClusterAddonLabelSourceName,
				),
			),
			builder.WithPredicates(predicate.ResourceVersionChangedPredicate{}),
		).
		Watches(
			&sourcev1.OCIRepository{},
			handler.EnqueueRequestsFromMapFunc(
				utils.MapInternalResources(
					ControllerName,
					helmv1alpha1.TargetNamespace,
					helmv1alpha1.LabelManagedBy,
					helmv1alpha1.LabelManagedByValue,
					helmv1alpha1.HelmClusterAddonLabelSourceName,
				),
			),
			builder.WithPredicates(predicate.ResourceVersionChangedPredicate{}),
		).
		Watches(
			&helmv1alpha1.HelmClusterAddonRepository{},
			handler.EnqueueRequestsFromMapFunc(utils.MapRepositoryToAddons(client)),
			builder.WithPredicates(predicate.GenerationChangedPredicate{}),
		).
		Watches(
			&helmv1alpha1.HelmClusterAddonChart{},
			handler.EnqueueRequestsFromMapFunc(utils.MapChartToAddons(client)),
			// A catalog write is a status-only change on the chart, so a
			// generation-only predicate (as used for HelmClusterAddonRepository
			// above) would never let it through; only a terminal probe verdict
			// being reversible depends on this watch firing here.
			builder.WithPredicates(predicate.ResourceVersionChangedPredicate{}),
		).
		Complete(r)
}
