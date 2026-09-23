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
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

func MapInternalResources(controllerName, targetNamespace, labelManagedBy, labelManagedByValue, labelSourceName string) handler.MapFunc {
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
			logger.V(1).Info("resource missing source label, skipping",
				"controller", controllerName, "watchedObject", client.ObjectKeyFromObject(obj))

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

// MapNamespacedInternalResources is MapInternalResources for a namespaced source
// kind: the request it enqueues carries the namespace recorded in
// labelSourceNamespace next to the name. Internal objects of every family live in
// targetNamespace, so the name alone would not identify a namespaced source. An
// object carrying only one of the two labels cannot be mapped and is skipped, at
// debug verbosity, because internal objects of the other families legitimately
// match the managed-by filter.
func MapNamespacedInternalResources(
	controllerName, targetNamespace, labelManagedBy, labelManagedByValue, labelSourceName, labelSourceNamespace string,
) handler.MapFunc {
	return func(ctx context.Context, obj client.Object) []reconcile.Request {
		logger := log.FromContext(ctx)

		if obj.GetNamespace() != targetNamespace {
			return nil
		}

		labels := obj.GetLabels()
		if labels[labelManagedBy] != labelManagedByValue {
			return nil
		}

		sourceName, sourceNamespace := labels[labelSourceName], labels[labelSourceNamespace]
		if sourceName == "" || sourceNamespace == "" {
			logger.V(1).Info("resource missing source labels, skipping",
				"controller", controllerName, "watchedObject", client.ObjectKeyFromObject(obj))

			return nil
		}

		return []reconcile.Request{
			{
				NamespacedName: types.NamespacedName{
					Name:      sourceName,
					Namespace: sourceNamespace,
				},
			},
		}
	}
}
