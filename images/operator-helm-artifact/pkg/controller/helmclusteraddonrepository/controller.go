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
	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
	"github.com/deckhouse/operator-helm/pkg/utils"
	sourcev1 "github.com/werf/nelm-source-controller/api/v1"
	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

func SetupWithManager(mgr ctrl.Manager) error {
	r := &Reconciler{
		Client: mgr.GetClient(),
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
			&sourcev1.OCIRepository{},
			handler.EnqueueRequestsFromMapFunc(utils.MapInternalToFacade(TargetNamespace, LabelManagedBy, LabelManagedByValue, LabelSourceName)),
			builder.WithPredicates(predicate.ResourceVersionChangedPredicate{}),
		).
		Watches(
			&corev1.Secret{},
			handler.EnqueueRequestsFromMapFunc(utils.MapInternalToFacade(TargetNamespace, LabelManagedBy, LabelManagedByValue, LabelSourceName)),
			builder.WithPredicates(predicate.ResourceVersionChangedPredicate{}),
		).
		Complete(r)
}
