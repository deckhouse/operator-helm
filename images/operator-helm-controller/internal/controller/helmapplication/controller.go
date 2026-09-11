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

// Package helmapplication wires the shared release reconciler to the HelmApplication
// kind: no chart claim, no target-namespace creation (the release deploys into its
// own namespace), and an identity of its own to apply the chart with. The kind can
// reference either repository kind of its family, so it watches both, and both
// catalogs.
package helmapplication

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
	ControllerName = "helmapplication-controller"
)

func SetupWithManager(mgr ctrl.Manager) error {
	client := mgr.GetClient()

	r := reconcile.New(client, reconcile.Deps{
		NewRelease:   adapter.EmptyApplicationRelease,
		Repositories: adapter.NewApplicationRepositoryResolver(client),
		Chart:        services.NewChartService(client, mgr.GetScheme(), helmv1alpha1.TargetNamespace),
		OCI:          services.NewOCIRepoService(client, mgr.GetScheme(), helmv1alpha1.TargetNamespace, nil),
		Release:      services.NewReleaseService(client, mgr.GetScheme(), helmv1alpha1.TargetNamespace),
		Maintenance:  services.NewMaintenanceService(client, mgr.GetScheme(), helmv1alpha1.TargetNamespace),
		Claim:        source.NoChartClaim{},
		Namespaces:   source.ExistingTargetNamespace{},
		Access:       services.NewAccessService(client, helmv1alpha1.TargetNamespace),
		Status:       status.NewManager(client),
	})

	mapInternal := utils.MapNamespacedInternalResources(
		ControllerName,
		helmv1alpha1.TargetNamespace,
		helmv1alpha1.LabelManagedBy,
		helmv1alpha1.LabelManagedByValue,
		helmv1alpha1.HelmApplicationLabelSourceName,
		helmv1alpha1.LabelSourceNamespace,
	)

	return ctrl.NewControllerManagedBy(mgr).
		Named(ControllerName).
		WithOptions(controller.Options{MaxConcurrentReconciles: 2}).
		For(
			&helmv1alpha1.HelmApplication{},
			builder.WithPredicates(predicate.Or(
				predicate.GenerationChangedPredicate{},
				predicate.AnnotationChangedPredicate{},
			)),
		).
		Watches(
			&sourcev1.HelmChart{},
			handler.EnqueueRequestsFromMapFunc(mapInternal),
			builder.WithPredicates(predicate.ResourceVersionChangedPredicate{}),
		).
		Watches(
			&helmv2.HelmRelease{},
			handler.EnqueueRequestsFromMapFunc(mapInternal),
			builder.WithPredicates(predicate.ResourceVersionChangedPredicate{}),
		).
		Watches(
			&sourcev1.OCIRepository{},
			handler.EnqueueRequestsFromMapFunc(mapInternal),
			builder.WithPredicates(predicate.ResourceVersionChangedPredicate{}),
		).
		Watches(
			&helmv1alpha1.HelmApplicationRepository{},
			handler.EnqueueRequestsFromMapFunc(utils.MapRepositoryToApplications(client, helmv1alpha1.HelmApplicationRepositoryKind)),
			builder.WithPredicates(predicate.GenerationChangedPredicate{}),
		).
		Watches(
			&helmv1alpha1.HelmClusterApplicationRepository{},
			handler.EnqueueRequestsFromMapFunc(utils.MapRepositoryToApplications(client, helmv1alpha1.HelmClusterApplicationRepositoryKind)),
			builder.WithPredicates(predicate.GenerationChangedPredicate{}),
		).
		// A catalog write is a status-only change, so a generation predicate would
		// never let it through; a terminal probe verdict being reversible depends on
		// these two watches firing.
		Watches(
			&helmv1alpha1.HelmApplicationChart{},
			handler.EnqueueRequestsFromMapFunc(utils.MapChartToApplications(client, helmv1alpha1.HelmApplicationRepositoryKind)),
			builder.WithPredicates(predicate.ResourceVersionChangedPredicate{}),
		).
		Watches(
			&helmv1alpha1.HelmClusterApplicationChart{},
			handler.EnqueueRequestsFromMapFunc(utils.MapChartToApplications(client, helmv1alpha1.HelmClusterApplicationRepositoryKind)),
			builder.WithPredicates(predicate.ResourceVersionChangedPredicate{}),
		).
		Complete(r)
}
