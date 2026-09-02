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

// AddonChart indexes HelmClusterAddon objects by the repository/chart pair they
// reference. The webhook uses it to enforce that a pair is claimed by one addon, and
// the repository synchronization uses it to find the addon that still references a
// chart before pruning anything.
const AddonChart = ".spec.chart.repoAndChart"

// AddonChartValue builds the index value of a repository/chart pair.
func AddonChartValue(repoName, chartName string) string {
	return repoName + "/" + chartName
}

// SetupAddonChart registers the AddonChart index on the manager's cache.
func SetupAddonChart(mgr ctrl.Manager) error {
	return mgr.GetFieldIndexer().IndexField(
		context.Background(), &helmv1alpha1.HelmClusterAddon{}, AddonChart,
		func(obj client.Object) []string {
			addon := obj.(*helmv1alpha1.HelmClusterAddon)

			return []string{AddonChartValue(
				addon.Spec.Chart.HelmClusterAddonRepository,
				addon.Spec.Chart.HelmClusterAddonChartName,
			)}
		},
	)
}
