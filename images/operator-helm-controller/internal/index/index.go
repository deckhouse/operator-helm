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

package index

import (
	"context"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
)

// AddonChart indexes HelmClusterAddon objects by the repository/chart pair they reference.
// The webhook and the repository synchronization both query it.
const AddonChart = ".spec.chart.repoAndChart"

// AddonChartValue builds the index value of a repository/chart pair.
func AddonChartValue(repoName, chartName string) string {
	return repoName + "/" + chartName
}

// AddonChartIndexer is the index function behind AddonChart. It is exported so tests and the
// webhook's fake client register the same function the manager's cache does.
func AddonChartIndexer(obj client.Object) []string {
	addon := obj.(*helmv1alpha1.HelmClusterAddon)

	return []string{AddonChartValue(
		addon.Spec.Chart.HelmClusterAddonRepository,
		addon.Spec.Chart.HelmClusterAddonChartName,
	)}
}

// SetupAddonChart registers the AddonChart index on the manager's cache.
func SetupAddonChart(mgr ctrl.Manager) error {
	return mgr.GetFieldIndexer().IndexField(
		context.Background(), &helmv1alpha1.HelmClusterAddon{}, AddonChart, AddonChartIndexer,
	)
}

// AddonRepository indexes HelmClusterAddon objects by the repository they reference.
const AddonRepository = ".spec.chart.helmClusterAddonRepository"

// AddonRepositoryIndexer is the index function behind AddonRepository.
func AddonRepositoryIndexer(obj client.Object) []string {
	addon := obj.(*helmv1alpha1.HelmClusterAddon)
	if addon.Spec.Chart.HelmClusterAddonRepository == "" {
		return nil
	}

	return []string{addon.Spec.Chart.HelmClusterAddonRepository}
}

// SetupAddonRepository registers the AddonRepository index on the manager's cache.
func SetupAddonRepository(mgr ctrl.Manager) error {
	return mgr.GetFieldIndexer().IndexField(
		context.Background(), &helmv1alpha1.HelmClusterAddon{}, AddonRepository, AddonRepositoryIndexer,
	)
}

// ApplicationRepository indexes HelmApplication objects by the repository they reference.
// The value carries the repository kind and namespace, not just the name.
const ApplicationRepository = ".spec.chart.repositoryRef"

// ApplicationRepositoryValue builds the index value of a repository reference. The
// namespace is empty for a cluster-scoped kind, which yields "<kind>//<name>".
func ApplicationRepositoryValue(kind, namespace, name string) string {
	return kind + "/" + namespace + "/" + name
}

// ApplicationChart indexes HelmApplication objects by the repository/chart pair they
// reference, with the repository identified the same way as in ApplicationRepository.
const ApplicationChart = ".spec.chart.repositoryAndChart"

// ApplicationChartValue builds the index value of a repository/chart pair.
func ApplicationChartValue(kind, namespace, name, chart string) string {
	return ApplicationRepositoryValue(kind, namespace, name) + "/" + chart
}

func applicationRepositoryRef(app *helmv1alpha1.HelmApplication) (kind, namespace, name string) {
	kind = app.RepositoryKind()
	if kind == helmv1alpha1.HelmApplicationRepositoryKind {
		namespace = app.Namespace
	}

	return kind, namespace, app.RepositoryName()
}

// ApplicationRepositoryIndexer is the index function behind ApplicationRepository. It
// is exported so tests can register the same function on a fake client.
func ApplicationRepositoryIndexer(obj client.Object) []string {
	app := obj.(*helmv1alpha1.HelmApplication)

	kind, namespace, name := applicationRepositoryRef(app)
	if kind == "" {
		return nil
	}

	return []string{ApplicationRepositoryValue(kind, namespace, name)}
}

// ApplicationChartIndexer is the index function behind ApplicationChart.
func ApplicationChartIndexer(obj client.Object) []string {
	app := obj.(*helmv1alpha1.HelmApplication)

	kind, namespace, name := applicationRepositoryRef(app)
	if kind == "" {
		return nil
	}

	return []string{ApplicationChartValue(kind, namespace, name, app.Spec.Chart.Name)}
}

// SetupApplicationRepository registers the ApplicationRepository index.
func SetupApplicationRepository(mgr ctrl.Manager) error {
	return mgr.GetFieldIndexer().IndexField(
		context.Background(), &helmv1alpha1.HelmApplication{}, ApplicationRepository, ApplicationRepositoryIndexer,
	)
}

// SetupApplicationChart registers the ApplicationChart index.
func SetupApplicationChart(mgr ctrl.Manager) error {
	return mgr.GetFieldIndexer().IndexField(
		context.Background(), &helmv1alpha1.HelmApplication{}, ApplicationChart, ApplicationChartIndexer,
	)
}
