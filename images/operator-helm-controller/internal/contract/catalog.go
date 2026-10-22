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

package contract

import (
	"context"

	"sigs.k8s.io/controller-runtime/pkg/client"

	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
	repoclient "github.com/deckhouse/operator-helm/internal/client/repository"
)

// CatalogResult reports what one catalog write left out. It is an outcome, not a
// verdict: turning it into conditions is the reconciler's job.
type CatalogResult struct {
	// SkippedCharts names the charts the repository offers that the catalog cannot hold.
	// The order is the order Reconcile saw them in, so a durable caller has to sort them.
	SkippedCharts []string
}

// Catalog mirrors the chart list of a repository into the chart catalog kind of its
// family. One Catalog serves every repository of one kind.
type Catalog interface {
	// Snapshot reads the catalog objects of one repository and returns the state a
	// synchronization pass decides from. It is a single read for the whole pass.
	Snapshot(ctx context.Context, repo Repository) (CatalogSnapshot, error)
	// InUseVersions reports the versions of one chart still referenced by the
	// consumers of this family. An empty chart name yields no versions.
	InUseVersions(ctx context.Context, repo Repository, chartName string) (map[string]struct{}, error)
	// Lookup returns the catalog object of one chart and its status. The error is a NotFound
	// when the repository does not offer the chart.
	Lookup(ctx context.Context, repo Repository, chartName string) (client.Object, *helmv1alpha1.ChartCatalogStatus, error)
}

// CatalogSnapshot is one read of a repository's catalog objects and everything a
// synchronization pass does with it.
type CatalogSnapshot interface {
	// Known returns the verdicts the snapshot holds, so the repository client can skip tags
	// a previous pass already examined. It is read after MarkForReexamination.
	Known() repoclient.KnownCharts
	// MarkForReexamination records on every version the catalog holds for the repository
	// that its verdict is no longer trusted, which is what forcing a refresh asks for.
	MarkForReexamination(ctx context.Context) error
	// Reconcile writes the fetched charts into the catalog and prunes objects the repository no
	// longer lists, keeping referenced versions. The result is meaningful next to an error too.
	Reconcile(ctx context.Context, charts []repoclient.Chart) (CatalogResult, error)
}
