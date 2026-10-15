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

// Package helmapplication wires the shared release reconciler to the HelmApplication kind:
// no chart claim, no target-namespace creation, and an identity of its own.
package helmapplication

import (
	helmv2 "github.com/fluxcd/helm-controller/api/v2"
	sourcev1 "github.com/fluxcd/source-controller/api/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
	"github.com/deckhouse/operator-helm/internal/adapter"
	"github.com/deckhouse/operator-helm/internal/internalobj"
	"github.com/deckhouse/operator-helm/internal/namespace"
	"github.com/deckhouse/operator-helm/internal/rbac"
	reconcile "github.com/deckhouse/operator-helm/internal/reconcile/release"
	"github.com/deckhouse/operator-helm/internal/status"
	"github.com/deckhouse/operator-helm/internal/utils"
)

const (
	ControllerName = "helmapplication-controller"
)

func SetupWithManager(mgr ctrl.Manager) error {
	client := mgr.GetClient()

	r := reconcile.New(client, reconcile.Deps{
		NewRelease:      adapter.EmptyApplicationRelease,
		Repositories:    adapter.NewApplicationRepositoryResolver(client),
		Charts:          internalobj.NewCharts(client, helmv1alpha1.TargetNamespace),
		OCIRepositories: internalobj.NewOCIRepositories(client, helmv1alpha1.TargetNamespace, nil),
		Releases:        internalobj.NewReleases(client, helmv1alpha1.TargetNamespace),
		Maintenance:     internalobj.NewMaintenance(client, helmv1alpha1.TargetNamespace),
		Claims:          reconcile.NoClaims{},
		Namespaces:      namespace.NewExistingTarget(mgr.GetAPIReader()),
		Identity:        rbac.NewIdentity(client, helmv1alpha1.TargetNamespace),
		Status:          status.NewManager(client),
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
		// Nothing but these two watches reports an out-of-band edit to the identity objects.
		Watches(
			&rbacv1.Role{},
			handler.EnqueueRequestsFromMapFunc(mapRoleToApplications(client)),
			builder.WithPredicates(predicate.ResourceVersionChangedPredicate{}),
		).
		Watches(
			&rbacv1.RoleBinding{},
			handler.EnqueueRequestsFromMapFunc(mapRoleBindingToApplications(client)),
			builder.WithPredicates(predicate.ResourceVersionChangedPredicate{}),
		).
		Watches(
			&helmv1alpha1.HelmApplicationRepository{},
			handler.EnqueueRequestsFromMapFunc(mapRepositoryToApplications(client, helmv1alpha1.HelmApplicationRepositoryKind)),
			builder.WithPredicates(predicate.GenerationChangedPredicate{}),
		).
		Watches(
			&helmv1alpha1.HelmClusterApplicationRepository{},
			handler.EnqueueRequestsFromMapFunc(mapRepositoryToApplications(client, helmv1alpha1.HelmClusterApplicationRepositoryKind)),
			builder.WithPredicates(predicate.GenerationChangedPredicate{}),
		).
		// A catalog write is status-only, so a generation predicate would never let it through.
		Watches(
			&helmv1alpha1.HelmApplicationChart{},
			handler.EnqueueRequestsFromMapFunc(mapChartToApplications(client, helmv1alpha1.HelmApplicationRepositoryKind)),
			builder.WithPredicates(predicate.ResourceVersionChangedPredicate{}),
		).
		Watches(
			&helmv1alpha1.HelmClusterApplicationChart{},
			handler.EnqueueRequestsFromMapFunc(mapChartToApplications(client, helmv1alpha1.HelmClusterApplicationRepositoryKind)),
			builder.WithPredicates(predicate.ResourceVersionChangedPredicate{}),
		).
		Complete(r)
}
