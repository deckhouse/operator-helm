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

package helmapplication

import (
	"context"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
	"github.com/deckhouse/operator-helm/internal/adapter"
	"github.com/deckhouse/operator-helm/internal/index"
	"github.com/deckhouse/operator-helm/internal/rbac"
)

// mapRoleToApplications enqueues every application of the namespace a change to the shared Role reaches.
func mapRoleToApplications(c client.Client) handler.MapFunc {
	return func(ctx context.Context, obj client.Object) []reconcile.Request {
		// Matched by name: a Role stripped of the managed-by label arrives here as a deletion.
		if obj.GetName() != rbac.ApplicationRoleName {
			return nil
		}

		apps, err := applicationsInNamespace(ctx, c, obj)
		if err != nil {
			return nil
		}

		requests := make([]reconcile.Request, 0, len(apps))
		for i := range apps {
			requests = append(requests, requestFor(&apps[i]))
		}

		return requests
	}
}

// mapRoleBindingToApplications enqueues the one application whose derived service account
// name the role binding is named after.
func mapRoleBindingToApplications(c client.Client) handler.MapFunc {
	return func(ctx context.Context, obj client.Object) []reconcile.Request {
		apps, err := applicationsInNamespace(ctx, c, obj)
		if err != nil {
			return nil
		}

		for i := range apps {
			if adapter.NewApplicationRelease(&apps[i]).InternalNames().ServiceAccount == obj.GetName() {
				return []reconcile.Request{requestFor(&apps[i])}
			}
		}

		return nil
	}
}

func applicationsInNamespace(ctx context.Context, c client.Client, obj client.Object) ([]helmv1alpha1.HelmApplication, error) {
	var apps helmv1alpha1.HelmApplicationList
	if err := c.List(ctx, &apps, client.InNamespace(obj.GetNamespace())); err != nil {
		log.FromContext(ctx).Error(err, "Failed to list HelmApplications for RBAC mapping",
			"controller", ControllerName, "watchedObject", client.ObjectKeyFromObject(obj))

		return nil, err
	}

	return apps.Items, nil
}

func requestFor(app *helmv1alpha1.HelmApplication) reconcile.Request {
	return reconcile.Request{NamespacedName: types.NamespacedName{Namespace: app.Namespace, Name: app.Name}}
}

// mapRepositoryToApplications enqueues the HelmApplication objects referencing a repository
// object of the given kind. The index value carries the kind and the repository namespace.
func mapRepositoryToApplications(c client.Client, repositoryKind string) handler.MapFunc {
	return func(ctx context.Context, obj client.Object) []reconcile.Request {
		var apps helmv1alpha1.HelmApplicationList
		if err := c.List(ctx, &apps, client.MatchingFields{
			index.ApplicationRepository: index.ApplicationRepositoryValue(repositoryKind, obj.GetNamespace(), obj.GetName()),
		}); err != nil {
			log.FromContext(ctx).Error(err, "Failed to list HelmApplications for repository mapping",
				"repositoryKind", repositoryKind, "watchedObject", client.ObjectKeyFromObject(obj))

			return nil
		}

		return applicationRequests(apps)
	}
}

// mapChartToApplications enqueues the HelmApplication objects using a chart catalog object of
// the family whose repository kind is given, found by its namespace and repository label.
func mapChartToApplications(c client.Client, repositoryKind string) handler.MapFunc {
	return func(ctx context.Context, obj client.Object) []reconcile.Request {
		labels := obj.GetLabels()
		repoName := labels[helmv1alpha1.LabelRepositoryName]
		chartName := labels[helmv1alpha1.LabelChartName]
		if repoName == "" || chartName == "" {
			log.FromContext(ctx).Info("Chart object missing repository or chart label, cannot map to applications",
				"repositoryKind", repositoryKind, "watchedObject", client.ObjectKeyFromObject(obj))

			return nil
		}

		var apps helmv1alpha1.HelmApplicationList
		if err := c.List(ctx, &apps, client.MatchingFields{
			index.ApplicationChart: index.ApplicationChartValue(repositoryKind, obj.GetNamespace(), repoName, chartName),
		}); err != nil {
			log.FromContext(ctx).Error(err, "Failed to list HelmApplications for chart mapping",
				"repositoryKind", repositoryKind, "watchedObject", client.ObjectKeyFromObject(obj),
				"repository", repoName, "chart", chartName)

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
