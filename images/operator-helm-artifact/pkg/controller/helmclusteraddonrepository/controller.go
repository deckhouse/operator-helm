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
	"context"

	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
	sourcev1 "github.com/werf/nelm-source-controller/api/v1"
)

// SetupWithManager registers the HelmClusterRepository controller with the manager.
func SetupWithManager(mgr ctrl.Manager) error {
	r := &Reconciler{
		Client: mgr.GetClient(),
	}

	return ctrl.NewControllerManagedBy(mgr).
		Named(ControllerName).
		// Primary watch: HelmClusterRepository (cluster-scoped).
		For(&helmv1alpha1.HelmClusterAddonRepository{}).
		// Secondary watch: HelmRepository in target namespace.
		// When the internal resource changes (e.g., status update from
		// nelm-source-controller), enqueue the parent HelmClusterRepository.
		Watches(
			&sourcev1.HelmRepository{},
			handler.EnqueueRequestsFromMapFunc(mapInternalToCluster),
		).
		Complete(r)
}

// mapInternalToCluster maps a HelmRepository event back to the
// HelmClusterRepository that owns it (by matching name and labels).
func mapInternalToCluster(ctx context.Context, obj client.Object) []reconcile.Request {
	logger := log.FromContext(ctx)

	// Only process resources in our target namespace.
	if obj.GetNamespace() != TargetNamespace {
		return nil
	}

	// Only process resources managed by this controller.
	labels := obj.GetLabels()
	if labels[LabelManagedBy] != LabelManagedByValue {
		return nil
	}

	sourceName := labels[LabelSourceName]
	if sourceName == "" {
		logger.Info("Internal repository resource missing source-name label, skipping",
			"name", obj.GetName(), "namespace", obj.GetNamespace())
		return nil
	}

	return []reconcile.Request{
		{
			NamespacedName: types.NamespacedName{
				// HelmClusterAddonRepository is cluster-scoped, so no namespace.
				Name: sourceName,
			},
		},
	}
}
