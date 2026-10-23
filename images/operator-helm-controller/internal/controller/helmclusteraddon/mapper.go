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
	"context"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
	"github.com/deckhouse/operator-helm/internal/index"
)

func mapRepositoryToAddons(c client.Client) handler.MapFunc {
	return func(ctx context.Context, obj client.Object) []reconcile.Request {
		addonList := &helmv1alpha1.HelmClusterAddonList{}
		if err := c.List(ctx, addonList, client.MatchingFields{index.AddonRepository: obj.GetName()}); err != nil {
			log.FromContext(ctx).Error(err, "Failed to list HelmClusterAddons for repository mapping",
				"watchedObject", client.ObjectKeyFromObject(obj))
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

// mapChartToAddons enqueues the addon that claims a HelmClusterAddonChart's repository/chart
// pair whenever the chart object changes.
func mapChartToAddons(c client.Client) handler.MapFunc {
	return func(ctx context.Context, obj client.Object) []reconcile.Request {
		labels := obj.GetLabels()
		repoName := labels[helmv1alpha1.LabelRepositoryName]
		chartName := labels[helmv1alpha1.LabelChartName]
		if repoName == "" || chartName == "" {
			// Without both labels there is no repository/chart pair to look an addon up by.
			log.FromContext(ctx).Info("Chart object missing repository or chart label, cannot map to addons",
				"watchedObject", client.ObjectKeyFromObject(obj))

			return nil
		}

		var addons helmv1alpha1.HelmClusterAddonList
		if err := c.List(ctx, &addons, client.MatchingFields{
			index.AddonChart: index.AddonChartValue(repoName, chartName),
		}); err != nil {
			log.FromContext(ctx).Error(err, "Failed to list HelmClusterAddons for chart mapping",
				"watchedObject", client.ObjectKeyFromObject(obj), "repository", repoName, "chart", chartName)

			return nil
		}

		requests := make([]reconcile.Request, 0, len(addons.Items))
		for _, addon := range addons.Items {
			requests = append(requests, reconcile.Request{NamespacedName: types.NamespacedName{Name: addon.Name}})
		}

		return requests
	}
}
