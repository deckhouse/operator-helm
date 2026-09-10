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

	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
	"github.com/deckhouse/operator-helm/internal/index"
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
				"controller", controllerName, "name", obj.GetName(), "namespace", obj.GetNamespace())

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
				"controller", controllerName, "name", obj.GetName(), "namespace", obj.GetNamespace())

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

func MapRepositoryToAddons(c client.Client) handler.MapFunc {
	return func(ctx context.Context, obj client.Object) []reconcile.Request {
		addonList := &helmv1alpha1.HelmClusterAddonList{}
		if err := c.List(ctx, addonList, client.MatchingFields{index.AddonRepository: obj.GetName()}); err != nil {
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

// MapChartToAddons enqueues the addon that claims a HelmClusterAddonChart's
// repository/chart pair whenever the chart object changes. This is the addon
// controller's only watch that can fire on a catalog write: after a terminal probe
// verdict (not a chart, or the tag no longer exists) the addon's internal HelmChart
// has already been removed and no OCIRepository was created for it, so none of the
// addon controller's other watches cover it, and without this one the addon would
// stay Ready=False until a human forces a reconcile even after the repository
// republishes something usable.
func MapChartToAddons(c client.Client) handler.MapFunc {
	return func(ctx context.Context, obj client.Object) []reconcile.Request {
		labels := obj.GetLabels()
		repoName := labels[helmv1alpha1.LabelRepositoryName]
		chartName := labels[helmv1alpha1.LabelChartName]
		if repoName == "" || chartName == "" {
			// Same fail-open tradeoff as knownCharts: without both labels there is no
			// repository/chart pair to look an addon up by, so this chart object
			// cannot be mapped back to anything.
			log.FromContext(ctx).Info("Chart object missing repository or chart label, cannot map to addons", "addonChartName", obj.GetName())

			return nil
		}

		var addons helmv1alpha1.HelmClusterAddonList
		if err := c.List(ctx, &addons, client.MatchingFields{
			index.AddonChart: index.AddonChartValue(repoName, chartName),
		}); err != nil {
			log.FromContext(ctx).Error(err, "Failed to list HelmClusterAddons for chart mapping")

			return nil
		}

		requests := make([]reconcile.Request, 0, len(addons.Items))
		for _, addon := range addons.Items {
			requests = append(requests, reconcile.Request{NamespacedName: types.NamespacedName{Name: addon.Name}})
		}

		return requests
	}
}

// MapRepositoryToApplications enqueues the HelmApplication objects referencing a
// repository object of the given kind. The index value carries the kind and the
// repository namespace, so a namespaced repository reaches only the applications of
// its own namespace and never a same-named repository's consumers elsewhere.
func MapRepositoryToApplications(c client.Client, repositoryKind string) handler.MapFunc {
	return func(ctx context.Context, obj client.Object) []reconcile.Request {
		var apps helmv1alpha1.HelmApplicationList
		if err := c.List(ctx, &apps, client.MatchingFields{
			index.ApplicationRepository: index.ApplicationRepositoryValue(repositoryKind, obj.GetNamespace(), obj.GetName()),
		}); err != nil {
			log.FromContext(ctx).Error(err, "Failed to list HelmApplications for repository mapping")

			return nil
		}

		return applicationRequests(apps)
	}
}

// MapChartToApplications enqueues the HelmApplication objects using a chart catalog
// object of the family whose repository kind is given. Like MapChartToAddons, this is
// the only watch that fires on a catalog write, which a terminal probe verdict being
// reversible depends on. The repository is identified by the catalog object's
// namespace and repository label.
func MapChartToApplications(c client.Client, repositoryKind string) handler.MapFunc {
	return func(ctx context.Context, obj client.Object) []reconcile.Request {
		labels := obj.GetLabels()
		repoName := labels[helmv1alpha1.LabelRepositoryName]
		chartName := labels[helmv1alpha1.LabelChartName]
		if repoName == "" || chartName == "" {
			log.FromContext(ctx).Info("Chart object missing repository or chart label, cannot map to applications", "name", obj.GetName(), "namespace", obj.GetNamespace())

			return nil
		}

		var apps helmv1alpha1.HelmApplicationList
		if err := c.List(ctx, &apps, client.MatchingFields{
			index.ApplicationChart: index.ApplicationChartValue(repositoryKind, obj.GetNamespace(), repoName, chartName),
		}); err != nil {
			log.FromContext(ctx).Error(err, "Failed to list HelmApplications for chart mapping")

			return nil
		}

		return applicationRequests(apps)
	}
}

func applicationRequests(apps helmv1alpha1.HelmApplicationList) []reconcile.Request {
	requests := make([]reconcile.Request, 0, len(apps.Items))
	for _, app := range apps.Items {
		requests = append(requests, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: app.Namespace, Name: app.Name}})
	}

	return requests
}
