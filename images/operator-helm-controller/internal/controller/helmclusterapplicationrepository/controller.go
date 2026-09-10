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

// Package helmclusterapplicationrepository wires the shared repository reconciler
// to the HelmClusterApplicationRepository kind.
package helmclusterapplicationrepository

import (
	sourcev1 "github.com/werf/nelm-source-controller/api/v1"
	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
	"github.com/deckhouse/operator-helm/internal/adapter"
	repoclient "github.com/deckhouse/operator-helm/internal/client/repository"
	"github.com/deckhouse/operator-helm/internal/manager/status"
	reconcile "github.com/deckhouse/operator-helm/internal/reconcile/repository"
	"github.com/deckhouse/operator-helm/internal/services"
	"github.com/deckhouse/operator-helm/internal/source"
	"github.com/deckhouse/operator-helm/internal/utils"
)

const (
	ControllerName = "helmclusterapplicationrepository-controller"
)

func SetupWithManager(mgr ctrl.Manager) error {
	client := mgr.GetClient()

	ociRepositoryService := services.NewOCIRepoService(client, mgr.GetScheme(), helmv1alpha1.TargetNamespace, nil)

	r := reconcile.New(
		client,
		adapter.EmptyClusterApplicationRepository,
		services.NewHelmRepoService(client, mgr.GetScheme(), helmv1alpha1.TargetNamespace),
		ociRepositoryService,
		// HelmApplication is not reconciled yet: a force request has no consumer
		// sources to reach. The HelmApplication controller replaces this.
		source.NoConsumers{},
		services.NewRepoSyncService(client, mgr.GetScheme(), repoclient.NewClient, adapter.NewClusterApplicationCatalog(client)),
		status.NewManager(client),
	)

	mapInternal := utils.MapInternalResources(
		ControllerName,
		helmv1alpha1.TargetNamespace,
		helmv1alpha1.LabelManagedBy,
		helmv1alpha1.LabelManagedByValue,
		helmv1alpha1.HelmClusterApplicationRepositoryLabelSourceName,
	)

	return ctrl.NewControllerManagedBy(mgr).
		Named(ControllerName).
		WithOptions(controller.Options{MaxConcurrentReconciles: 2}).
		For(
			&helmv1alpha1.HelmClusterApplicationRepository{},
			builder.WithPredicates(predicate.Or(
				predicate.GenerationChangedPredicate{},
				predicate.AnnotationChangedPredicate{},
			)),
		).
		Watches(
			&sourcev1.HelmRepository{},
			handler.EnqueueRequestsFromMapFunc(mapInternal),
			builder.WithPredicates(predicate.ResourceVersionChangedPredicate{}),
		).
		Watches(
			&corev1.Secret{},
			handler.EnqueueRequestsFromMapFunc(mapInternal),
			builder.WithPredicates(predicate.ResourceVersionChangedPredicate{}),
		).
		Watches(
			&helmv1alpha1.HelmClusterApplicationChart{},
			handler.EnqueueRequestForOwner(
				mgr.GetScheme(),
				mgr.GetRESTMapper(),
				&helmv1alpha1.HelmClusterApplicationRepository{},
				handler.OnlyControllerOwner(),
			),
		).Complete(r)
}
