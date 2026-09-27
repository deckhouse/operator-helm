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
	// SkippedCharts names the charts the repository offers that the catalog cannot
	// hold. They are not failures — the pass completes without them — but they are
	// exactly the difference between the charts the repository offers and the charts
	// this pass published, so a caller that reports either number has to know about
	// them. The order is whatever order the charts were handed to Reconcile in, which
	// for a Helm index is map iteration order and therefore differs from pass to
	// pass; a caller that puts these names anywhere durable has to sort them.
	SkippedCharts []string
}

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
	// MarkForReexamination records on every version the catalog holds for the
	// repository that its verdict is no longer trusted, which is what forcing a
	// refresh asks for. It is the durable form of that request: the annotation
	// carrying it is consumed by the pass that acts on it, so the request has to be
	// in the catalog before the registry is contacted, or a read that fails takes the
	// whole refresh with it and the re-pushed tag it was asked for is never looked at
	// again. Every later pass — forced or not — re-examines a marked version and
	// clears the mark by answering it.
	MarkForReexamination(ctx context.Context, repo Repository) error
	// Reconcile writes the fetched charts into the catalog and prunes objects the
	// repository no longer lists, keeping any version a consumer still references.
	// The result names the charts it could not publish; it is filled in as far as
	// the pass got, so it is meaningful next to an error too.
	Reconcile(ctx context.Context, repo Repository, charts []repoclient.Chart) (CatalogResult, error)
	// InUseVersions reports the versions of one chart still referenced by the
	// consumers of this family. An empty chart name yields no versions.
	InUseVersions(ctx context.Context, repo Repository, chartName string) (map[string]struct{}, error)
	// Lookup returns the catalog object of one chart and its status, so a release
	// can find the version it asks for. The error is a NotFound when the repository
	// does not offer the chart.
	Lookup(ctx context.Context, repo Repository, chartName string) (client.Object, *helmv1alpha1.ChartCatalogStatus, error)
}
