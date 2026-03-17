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

package utils

import (
	"context"

	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
)

func MapInternalResources(targetNamespace, labelManagedBy, labelManagedByValue, labelSourceName string) handler.MapFunc {
	return func(ctx context.Context, obj client.Object) []reconcile.Request {
		logger := log.FromContext(ctx)

		if obj.GetNamespace() != targetNamespace {
			return nil
		}

		labels := obj.GetLabels()
		if labels[labelManagedBy] != labelManagedByValue {
			return nil
		}

		sourceName := labels[labelSourceName]
		if sourceName == "" {
			logger.Info("resource missing source label, skipping",
				"name", obj.GetName(), "namespace", obj.GetNamespace())

			return nil
		}

		return []reconcile.Request{
			{
				NamespacedName: types.NamespacedName{
					Name:      sourceName,
					Namespace: "",
				},
			},
		}
	}
}

const AddonRepositoryIndex = ".spec.chart.helmClusterAddonRepository"

func SetupAddonRepositoryIndex(mgr ctrl.Manager) error {
	return mgr.GetFieldIndexer().IndexField(context.Background(), &helmv1alpha1.HelmClusterAddon{}, AddonRepositoryIndex,
		func(obj client.Object) []string {
			addon := obj.(*helmv1alpha1.HelmClusterAddon)
			if addon.Spec.Chart.HelmClusterAddonRepository == "" {
				return nil
			}
			return []string{addon.Spec.Chart.HelmClusterAddonRepository}
		},
	)
}

func MapRepositoryToAddons(c client.Client) handler.MapFunc {
	return func(ctx context.Context, obj client.Object) []reconcile.Request {
		addonList := &helmv1alpha1.HelmClusterAddonList{}
		if err := c.List(ctx, addonList, client.MatchingFields{AddonRepositoryIndex: obj.GetName()}); err != nil {
			log.FromContext(ctx).Error(err, "Failed to list HelmClusterAddons for repository mapping")
			return nil
		}

		requests := make([]reconcile.Request, 0, len(addonList.Items))
		for _, addon := range addonList.Items {
			requests = append(requests, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: addon.Name},
			})
		}
		return requests
	}
}
