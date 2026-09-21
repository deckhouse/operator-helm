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
	"github.com/deckhouse/operator-helm/internal/services"
)

// mapRoleToApplications enqueues every application of the namespace a change to the
// namespace Role reaches. They all deploy as subjects bound to that one Role, so an
// edit to it concerns all of them: any of them writes back a narrowing, and any of
// them reports a Role that stopped being ours.
//
// The object is matched by name rather than by label. The informer behind this watch
// selects on the managed-by label, so a Role stripped of it leaves the informer and
// arrives here as a deletion — a state the object may equally reach through a
// tombstone, and the name is the one thing every form of the event carries. That
// event is the only notice such a Role ever produces: one that never carried the
// label is invisible to the informer, so its arrival and its removal both pass
// unseen and the application finds it on a pass it runs for another reason.
func mapRoleToApplications(c client.Client) handler.MapFunc {
	return func(ctx context.Context, obj client.Object) []reconcile.Request {
		if obj.GetName() != services.ApplicationRoleName {
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

// mapRoleBindingToApplications enqueues the one application whose identity a role
// binding carries. The binding is named after the application's service account,
// whose name is derived from the application, so the applications of the binding's
// namespace are matched on their own derived name — again rather than on the labels
// the binding carries, for the reason above.
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
		log.FromContext(ctx).Error(err, "Failed to list HelmApplications for access mapping",
			"controller", ControllerName, "watchedObject", client.ObjectKeyFromObject(obj))

		return nil, err
	}

	return apps.Items, nil
}

func requestFor(app *helmv1alpha1.HelmApplication) reconcile.Request {
	return reconcile.Request{NamespacedName: types.NamespacedName{Namespace: app.Namespace, Name: app.Name}}
}

// mapRepositoryToApplications enqueues the HelmApplication objects referencing a
// repository object of the given kind. The index value carries the kind and the
// repository namespace, so a namespaced repository reaches only the applications of
// its own namespace and never a same-named repository's consumers elsewhere.
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

// mapChartToApplications enqueues the HelmApplication objects using a chart catalog
// object of the family whose repository kind is given. Like mapChartToAddons, this is
// the only watch that fires on a catalog write, which a terminal probe verdict being
// reversible depends on. The repository is identified by the catalog object's
// namespace and repository label.
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
