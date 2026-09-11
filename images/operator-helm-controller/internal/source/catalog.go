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

package source

import (
	"context"

	"sigs.k8s.io/controller-runtime/pkg/client"

	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
	repoclient "github.com/deckhouse/operator-helm/internal/client/repository"
)

// Catalog mirrors the chart list of a repository into the chart catalog kind of
// its family. One Catalog serves every repository of one kind; the repository is
// passed to each call because the objects it writes are owned by, labelled with
// and (for a namespaced kind) placed next to that repository.
type Catalog interface {
	// Known returns the verdicts recorded for the repository by previous passes,
	// so the repository client can skip tags it already examined.
	Known(ctx context.Context, repo Repository) (repoclient.KnownCharts, error)
	// MigrateNames moves the repository's catalog objects to the names the current
	// scheme derives. It reads nothing from the repository, so it must run on every
	// reconcile, not only when a fetch is attempted or succeeds.
	//
	// TRANSITIONAL: remove once every cluster has reconciled each repository once
	// under the current scheme.
	MigrateNames(ctx context.Context, repo Repository) error
	// Reconcile writes the fetched charts into the catalog and prunes objects the
	// repository no longer lists, keeping any version a consumer still references.
	Reconcile(ctx context.Context, repo Repository, charts []repoclient.Chart) error
	// InUseVersions reports the versions of one chart still referenced by the
	// consumers of this family. An empty chart name yields no versions.
	InUseVersions(ctx context.Context, repo Repository, chartName string) (map[string]struct{}, error)
	// Lookup returns the catalog object of one chart and its status, so a release
	// can find the version it asks for. The error is a NotFound when the repository
	// does not offer the chart.
	Lookup(ctx context.Context, repo Repository, chartName string) (client.Object, *helmv1alpha1.ChartCatalogStatus, error)
}
