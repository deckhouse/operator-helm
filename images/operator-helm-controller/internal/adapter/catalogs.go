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

package adapter

import (
	"context"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/deckhouse/operator-helm/api/naming"
	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
	"github.com/deckhouse/operator-helm/internal/catalog"
	"github.com/deckhouse/operator-helm/internal/index"
	"github.com/deckhouse/operator-helm/internal/source"
)

// NewAddonCatalog builds the HelmClusterAddonChart catalog. Its consumers are the
// HelmClusterAddon objects referencing a repository/chart pair.
func NewAddonCatalog(c client.Client) source.Catalog {
	return catalog.New(c, catalog.Config[*helmv1alpha1.HelmClusterAddonChart, *helmv1alpha1.HelmClusterAddonChartList]{
		Kind:      helmv1alpha1.HelmClusterAddonChartKind,
		NewObject: func() *helmv1alpha1.HelmClusterAddonChart { return &helmv1alpha1.HelmClusterAddonChart{} },
		NewList:   func() *helmv1alpha1.HelmClusterAddonChartList { return &helmv1alpha1.HelmClusterAddonChartList{} },
		Items: func(l *helmv1alpha1.HelmClusterAddonChartList) []*helmv1alpha1.HelmClusterAddonChart {
			return pointers(l.Items)
		},
		Status:     func(o *helmv1alpha1.HelmClusterAddonChart) *helmv1alpha1.ChartCatalogStatus { return &o.Status },
		ObjectName: naming.HelmClusterAddonChartName,
		Consumers:  addonChartConsumers(c),
	})
}

// NewApplicationCatalog builds the HelmApplicationChart catalog. Consumers is nil
// until the HelmApplication controller exists: nothing can reference a chart yet.
func NewApplicationCatalog(c client.Client) source.Catalog {
	return catalog.New(c, catalog.Config[*helmv1alpha1.HelmApplicationChart, *helmv1alpha1.HelmApplicationChartList]{
		Kind:      helmv1alpha1.HelmApplicationChartKind,
		NewObject: func() *helmv1alpha1.HelmApplicationChart { return &helmv1alpha1.HelmApplicationChart{} },
		NewList:   func() *helmv1alpha1.HelmApplicationChartList { return &helmv1alpha1.HelmApplicationChartList{} },
		Items: func(l *helmv1alpha1.HelmApplicationChartList) []*helmv1alpha1.HelmApplicationChart {
			return pointers(l.Items)
		},
		Status:     func(o *helmv1alpha1.HelmApplicationChart) *helmv1alpha1.ChartCatalogStatus { return &o.Status },
		ObjectName: naming.ApplicationChartName,
	})
}

// NewClusterApplicationCatalog builds the HelmClusterApplicationChart catalog.
// Consumers is nil for the same reason as in NewApplicationCatalog.
func NewClusterApplicationCatalog(c client.Client) source.Catalog {
	return catalog.New(c, catalog.Config[*helmv1alpha1.HelmClusterApplicationChart, *helmv1alpha1.HelmClusterApplicationChartList]{
		Kind:      helmv1alpha1.HelmClusterApplicationChartKind,
		NewObject: func() *helmv1alpha1.HelmClusterApplicationChart { return &helmv1alpha1.HelmClusterApplicationChart{} },
		NewList: func() *helmv1alpha1.HelmClusterApplicationChartList {
			return &helmv1alpha1.HelmClusterApplicationChartList{}
		},
		Items: func(l *helmv1alpha1.HelmClusterApplicationChartList) []*helmv1alpha1.HelmClusterApplicationChart {
			return pointers(l.Items)
		},
		Status:     func(o *helmv1alpha1.HelmClusterApplicationChart) *helmv1alpha1.ChartCatalogStatus { return &o.Status },
		ObjectName: naming.ClusterApplicationChartName,
	})
}

// pointers returns a pointer to every element of items, so a caller can mutate the
// listed objects in place.
func pointers[T any](items []T) []*T {
	out := make([]*T, len(items))
	for i := range items {
		out[i] = &items[i]
	}

	return out
}

// addonChartConsumers returns the chart versions referenced by the addon that uses
// a repository/chart pair. The webhook and the claim Lease enforce one addon per
// pair, so at most one is found; both its desired and its last applied version
// count, since they differ during an upgrade.
func addonChartConsumers(c client.Client) func(context.Context, source.Repository, string) (map[string]struct{}, error) {
	return func(ctx context.Context, repo source.Repository, chartName string) (map[string]struct{}, error) {
		var addons helmv1alpha1.HelmClusterAddonList
		if err := c.List(ctx, &addons, client.MatchingFields{
			index.AddonChart: index.AddonChartValue(repo.Name(), chartName),
		}); err != nil {
			return nil, fmt.Errorf("listing addons of chart %q: %w", chartName, err)
		}

		inUse := make(map[string]struct{}, 2)

		for _, addon := range addons.Items {
			inUse[addon.Spec.Chart.Version] = struct{}{}

			// LastAppliedChart carries its own repository/chart identity and can lag
			// behind Spec.Chart when an addon is switched to a different chart: only
			// credit it here when it still names this repository/chart pair, or a
			// stale entry would protect a phantom version on the new chart while no
			// longer protecting the version actually applied on the old one.
			if last := addon.Status.LastAppliedChart; last != nil &&
				last.HelmClusterAddonChartName == chartName && last.HelmClusterAddonRepository == repo.Name() {
				inUse[last.Version] = struct{}{}
			}
		}

		return inUse, nil
	}
}
