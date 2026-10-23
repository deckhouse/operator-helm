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

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/deckhouse/operator-helm/api/naming"
	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
	"github.com/deckhouse/operator-helm/internal/catalog"
	"github.com/deckhouse/operator-helm/internal/contract"
)

// NewAddonCatalog builds the HelmClusterAddonChart catalog. Its consumers are the
// HelmClusterAddon objects referencing a repository/chart pair.
func NewAddonCatalog(c client.Client, reader client.Reader) contract.Catalog {
	return catalog.New(c, reader, catalog.Config[*helmv1alpha1.HelmClusterAddonChart, *helmv1alpha1.HelmClusterAddonChartList]{
		Kind:      helmv1alpha1.HelmClusterAddonChartKind,
		NewObject: func() *helmv1alpha1.HelmClusterAddonChart { return &helmv1alpha1.HelmClusterAddonChart{} },
		NewList:   func() *helmv1alpha1.HelmClusterAddonChartList { return &helmv1alpha1.HelmClusterAddonChartList{} },
		Items: func(l *helmv1alpha1.HelmClusterAddonChartList) []*helmv1alpha1.HelmClusterAddonChart {
			return pointers(l.Items)
		},
		Status:     func(o *helmv1alpha1.HelmClusterAddonChart) *helmv1alpha1.ChartCatalogStatus { return &o.Status },
		ObjectName: naming.HelmClusterAddonChartName,
		Consumers:  chartConsumers(ListAddonReleases(c)),
	})
}

// NewApplicationCatalog builds the HelmApplicationChart catalog. Its consumers are the
// HelmApplication objects of the repository's namespace.
func NewApplicationCatalog(c client.Client, reader client.Reader) contract.Catalog {
	return catalog.New(c, reader, catalog.Config[*helmv1alpha1.HelmApplicationChart, *helmv1alpha1.HelmApplicationChartList]{
		Kind:      helmv1alpha1.HelmApplicationChartKind,
		NewObject: func() *helmv1alpha1.HelmApplicationChart { return &helmv1alpha1.HelmApplicationChart{} },
		NewList:   func() *helmv1alpha1.HelmApplicationChartList { return &helmv1alpha1.HelmApplicationChartList{} },
		Items: func(l *helmv1alpha1.HelmApplicationChartList) []*helmv1alpha1.HelmApplicationChart {
			return pointers(l.Items)
		},
		Status:     func(o *helmv1alpha1.HelmApplicationChart) *helmv1alpha1.ChartCatalogStatus { return &o.Status },
		ObjectName: naming.HelmApplicationChartName,
		Consumers:  chartConsumers(ListApplicationReleases(c)),
	})
}

// NewClusterApplicationCatalog builds the HelmClusterApplicationChart catalog. Its consumers
// are the HelmApplication objects of every namespace.
func NewClusterApplicationCatalog(c client.Client, reader client.Reader) contract.Catalog {
	return catalog.New(c, reader, catalog.Config[*helmv1alpha1.HelmClusterApplicationChart, *helmv1alpha1.HelmClusterApplicationChartList]{
		Kind:      helmv1alpha1.HelmClusterApplicationChartKind,
		NewObject: func() *helmv1alpha1.HelmClusterApplicationChart { return &helmv1alpha1.HelmClusterApplicationChart{} },
		NewList: func() *helmv1alpha1.HelmClusterApplicationChartList {
			return &helmv1alpha1.HelmClusterApplicationChartList{}
		},
		Items: func(l *helmv1alpha1.HelmClusterApplicationChartList) []*helmv1alpha1.HelmClusterApplicationChart {
			return pointers(l.Items)
		},
		Status:     func(o *helmv1alpha1.HelmClusterApplicationChart) *helmv1alpha1.ChartCatalogStatus { return &o.Status },
		ObjectName: naming.HelmClusterApplicationChartName,
		Consumers:  chartConsumers(ListApplicationReleases(c)),
	})
}

func pointers[T any](items []T) []*T {
	out := make([]*T, len(items))
	for i := range items {
		out[i] = &items[i]
	}

	return out
}

// chartConsumers derives the in-use versions of a chart from the releases that reference it.
func chartConsumers(list contract.ReleaseLister) func(context.Context, contract.Repository, string) (map[string]struct{}, error) {
	return func(ctx context.Context, repo contract.Repository, chartName string) (map[string]struct{}, error) {
		releases, err := list(ctx, repo, chartName)
		if err != nil {
			return nil, err
		}

		pair := contract.RepositoryRef{Kind: repo.OwnerGVK().Kind, Namespace: repo.Namespace(), Name: repo.Name()}
		inUse := make(map[string]struct{}, 2*len(releases))

		for _, rel := range releases {
			inUse[rel.ChartRef().Version] = struct{}{}

			// LastAppliedChart can lag behind the spec, so it counts only while it still names this pair.
			if last := rel.LastAppliedChart(); last != nil && last.Chart == chartName && last.Repository == pair {
				inUse[last.Version] = struct{}{}
			}
		}

		return inUse, nil
	}
}
