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

package catalog_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/Masterminds/semver/v3"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/deckhouse/operator-helm/api/naming"
	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
	"github.com/deckhouse/operator-helm/internal/adapter"
	repoclient "github.com/deckhouse/operator-helm/internal/client/repository"
	"github.com/deckhouse/operator-helm/internal/contract"
	"github.com/deckhouse/operator-helm/internal/index"
)

func newClient(t *testing.T) client.Client {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := helmv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("registering helm scheme: %v", err)
	}

	return fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&helmv1alpha1.HelmApplicationChart{}, &helmv1alpha1.HelmClusterApplicationChart{}).
		WithIndex(&helmv1alpha1.HelmApplication{}, index.ApplicationRepository, index.ApplicationRepositoryIndexer).
		WithIndex(&helmv1alpha1.HelmApplication{}, index.ApplicationChart, index.ApplicationChartIndexer).
		Build()
}

func applicationRepo(namespace, name string) contract.Repository { //nolint:unparam // the parameter names the value the assertions read; inlining it would hide what the fixture stands for
	return adapter.NewApplicationRepository(&helmv1alpha1.HelmApplicationRepository{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, UID: types.UID(namespace + "/" + name)},
		Spec:       helmv1alpha1.RepositorySpec{URL: "https://charts.example.invalid/" + name},
	})
}

// newAddonClient builds a client for the addon family's own tests, indexed the way
// the addon consumer lookup (chartConsumers over ListAddonReleases) requires.
func newAddonClient(t *testing.T, objects ...client.Object) client.WithWatch {
	t.Helper()

	return newAddonClientWithInterceptor(t, interceptor.Funcs{}, objects...)
}

func newAddonClientWithInterceptor(t *testing.T, funcs interceptor.Funcs, objects ...client.Object) client.WithWatch {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := helmv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("registering helm scheme: %v", err)
	}

	return fake.NewClientBuilder().
		WithScheme(scheme).
		WithInterceptorFuncs(funcs).
		WithStatusSubresource(&helmv1alpha1.HelmClusterAddonChart{}).
		WithObjects(objects...).
		WithIndex(&helmv1alpha1.HelmClusterAddon{}, index.AddonChart, func(obj client.Object) []string {
			addon := obj.(*helmv1alpha1.HelmClusterAddon)

			return []string{index.AddonChartValue(addon.Spec.Chart.HelmClusterAddonRepository, addon.Spec.Chart.HelmClusterAddonChartName)}
		}).
		WithIndex(&helmv1alpha1.HelmClusterAddon{}, index.AddonRepository, func(obj client.Object) []string {
			addon := obj.(*helmv1alpha1.HelmClusterAddon)

			return []string{addon.Spec.Chart.HelmClusterAddonRepository}
		}).
		Build()
}

func addonRepo() contract.Repository {
	return adapter.NewAddonRepository(&helmv1alpha1.HelmClusterAddonRepository{ObjectMeta: metav1.ObjectMeta{Name: "example"}})
}

func addonConsumer(name, repoName, chartName, version string) *helmv1alpha1.HelmClusterAddon { //nolint:unparam // the parameter names the value the assertions read; inlining it would hide what the fixture stands for
	return &helmv1alpha1.HelmClusterAddon{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: helmv1alpha1.HelmClusterAddonSpec{
			Namespace: "app",
			Chart: helmv1alpha1.HelmClusterAddonChartRef{
				HelmClusterAddonRepository: repoName,
				HelmClusterAddonChartName:  chartName,
				Version:                    version,
			},
		},
	}
}

// legacyAddonChart builds a HelmClusterAddonChart under a name that predates the
// current scheme, labelled the way every catalog object is labelled. The name is a
// plain literal, not a recomputation of any past scheme: the migration finds this
// object by its chart label alone.
func legacyAddonChart(name, repoName, chartName string, versions ...helmv1alpha1.ChartVersion) *helmv1alpha1.HelmClusterAddonChart { //nolint:unparam // the parameter names the value the assertions read; inlining it would hide what the fixture stands for
	return &helmv1alpha1.HelmClusterAddonChart{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				helmv1alpha1.LabelDeckhouseHeritage: helmv1alpha1.LabelDeckhouseHeritageValue,
				helmv1alpha1.LabelRepositoryName:    repoName,
				helmv1alpha1.LabelChartName:         chartName,
			},
		},
		Status: helmv1alpha1.ChartCatalogStatus{Versions: versions},
	}
}

// syncCatalog runs the two steps the repository reconciler runs, in its order and
// with its gate: the rename first, independent of any fetch, and the catalog write
// only if the rename finished. The gate is what keeps an unfinished rename from
// being written over; it lives in the reconciler, so the test that owns it is
// TestUnfinishedRenameDoesNotOverwriteTheCatalog in internal/reconcile/repository,
// not here.
func syncCatalog(cat contract.Catalog, repo contract.Repository, charts ...repoclient.Chart) error {
	if err := cat.MigrateNames(context.Background(), repo); err != nil {
		return err
	}

	_, err := cat.Reconcile(context.Background(), repo, charts)

	return err
}

func chart(name, version string) repoclient.Chart {
	return repoclient.Chart{
		Name:     name,
		Versions: []repoclient.ChartVersion{{Version: semver.MustParse(version), IconURL: "https://example.invalid/" + name + ".png"}},
	}
}

func TestReconcileWritesNamespacedCatalogNextToTheRepository(t *testing.T) {
	c := newClient(t)
	cat := adapter.NewApplicationCatalog(c)
	repo := applicationRepo("team-a", "stable")

	if _, err := cat.Reconcile(context.Background(), repo, []repoclient.Chart{chart("podinfo", "6.7.1")}); err != nil {
		t.Fatalf("Reconcile returned %v", err)
	}

	got := &helmv1alpha1.HelmApplicationChart{}
	key := client.ObjectKey{Namespace: "team-a", Name: naming.ApplicationChartName("stable", "podinfo")}
	if err := c.Get(context.Background(), key, got); err != nil {
		t.Fatalf("chart object was not created in the repository namespace: %v", err)
	}

	if got.Labels[helmv1alpha1.LabelRepositoryName] != "stable" || got.Labels[helmv1alpha1.LabelChartName] != "podinfo" {
		t.Fatalf("labels = %v, want repository=stable chart=podinfo", got.Labels)
	}
	if got.Labels[helmv1alpha1.LabelDeckhouseHeritage] != helmv1alpha1.LabelDeckhouseHeritageValue {
		t.Fatalf("heritage label = %q", got.Labels[helmv1alpha1.LabelDeckhouseHeritage])
	}

	if len(got.OwnerReferences) != 1 {
		t.Fatalf("owner references = %v, want exactly one", got.OwnerReferences)
	}
	owner := got.OwnerReferences[0]
	if owner.Kind != helmv1alpha1.HelmApplicationRepositoryKind || owner.Name != "stable" || owner.Controller == nil || !*owner.Controller {
		t.Fatalf("owner = %+v, want a controller reference to HelmApplicationRepository/stable", owner)
	}

	if len(got.Status.Versions) != 1 || got.Status.Versions[0].Version != "6.7.1" {
		t.Fatalf("versions = %+v, want [6.7.1]", got.Status.Versions)
	}
	if got.Status.IconURL != "https://example.invalid/podinfo.png" {
		t.Fatalf("icon = %q", got.Status.IconURL)
	}
}

// TestSameNamedRepositoriesKeepSeparateCatalogs is why the namespaced catalog
// lists by namespace: two repositories named "stable" in different namespaces
// produce same-named chart objects, and one repository's pruning must not see the
// other's.
func TestSameNamedRepositoriesKeepSeparateCatalogs(t *testing.T) {
	c := newClient(t)
	cat := adapter.NewApplicationCatalog(c)
	teamA := applicationRepo("team-a", "stable")
	teamB := applicationRepo("team-b", "stable")

	if _, err := cat.Reconcile(context.Background(), teamA, []repoclient.Chart{chart("podinfo", "1.0.0")}); err != nil {
		t.Fatalf("Reconcile(team-a) returned %v", err)
	}
	if _, err := cat.Reconcile(context.Background(), teamB, []repoclient.Chart{chart("nginx", "2.0.0")}); err != nil {
		t.Fatalf("Reconcile(team-b) returned %v", err)
	}

	known, err := cat.Known(context.Background(), teamA)
	if err != nil {
		t.Fatalf("Known returned %v", err)
	}
	if _, ok := known["podinfo"]; !ok || len(known) != 1 {
		t.Fatalf("Known(team-a) = %v, want only podinfo", known)
	}

	if _, err := cat.Reconcile(context.Background(), teamA, nil); err != nil {
		t.Fatalf("Reconcile(team-a, empty) returned %v", err)
	}

	var charts helmv1alpha1.HelmApplicationChartList
	if err := c.List(context.Background(), &charts); err != nil {
		t.Fatalf("listing charts: %v", err)
	}
	if len(charts.Items) != 1 || charts.Items[0].Namespace != "team-b" {
		t.Fatalf("remaining charts = %v, want only team-b's", charts.Items)
	}
}

func TestClusterCatalogObjectsHaveNoNamespace(t *testing.T) {
	c := newClient(t)
	cat := adapter.NewClusterApplicationCatalog(c)
	repo := adapter.NewClusterApplicationRepository(&helmv1alpha1.HelmClusterApplicationRepository{
		ObjectMeta: metav1.ObjectMeta{Name: "shared", UID: "shared"},
		Spec:       helmv1alpha1.RepositorySpec{URL: "oci://ghcr.io/example/charts"},
	})

	if _, err := cat.Reconcile(context.Background(), repo, []repoclient.Chart{chart("podinfo", "6.7.1")}); err != nil {
		t.Fatalf("Reconcile returned %v", err)
	}

	got := &helmv1alpha1.HelmClusterApplicationChart{}
	key := client.ObjectKey{Name: naming.ClusterApplicationChartName("shared", "podinfo")}
	if err := c.Get(context.Background(), key, got); err != nil {
		t.Fatalf("cluster chart object was not created: %v", err)
	}
	if got.OwnerReferences[0].Kind != helmv1alpha1.HelmClusterApplicationRepositoryKind {
		t.Fatalf("owner kind = %q", got.OwnerReferences[0].Kind)
	}
}

// TestUnreferencedVersionsArePruned pins what happens to a version nothing uses.
// The application catalogs do have consumers — the HelmApplication objects of the
// repository — but this fixture creates none, so no version is protected and every
// unlisted one goes away.
func TestUnreferencedVersionsArePruned(t *testing.T) {
	c := newClient(t)
	cat := adapter.NewApplicationCatalog(c)
	repo := applicationRepo("team-a", "stable")

	if _, err := cat.Reconcile(context.Background(), repo, []repoclient.Chart{chart("podinfo", "1.0.0"), chart("nginx", "1.0.0")}); err != nil {
		t.Fatalf("first Reconcile returned %v", err)
	}
	if err := syncCatalog(cat, repo, chart("podinfo", "1.0.0")); err != nil {
		t.Fatalf("second Reconcile returned %v", err)
	}

	var charts helmv1alpha1.HelmApplicationChartList
	if err := c.List(context.Background(), &charts, client.InNamespace("team-a")); err != nil {
		t.Fatalf("listing charts: %v", err)
	}
	if len(charts.Items) != 1 || charts.Items[0].Labels[helmv1alpha1.LabelChartName] != "podinfo" {
		t.Fatalf("remaining charts = %v, want only podinfo", charts.Items)
	}

	inUse, err := cat.InUseVersions(context.Background(), repo, "podinfo")
	if err != nil {
		t.Fatalf("InUseVersions returned %v", err)
	}
	if len(inUse) != 0 {
		t.Fatalf("InUseVersions = %v, want none without consumers", inUse)
	}
}

func TestLookupReturnsTheCatalogObjectAndItsStatus(t *testing.T) {
	c := newClient(t)
	cat := adapter.NewApplicationCatalog(c)
	repo := applicationRepo("team-a", "stable")

	if _, err := cat.Reconcile(context.Background(), repo, []repoclient.Chart{chart("podinfo", "6.7.1")}); err != nil {
		t.Fatalf("Reconcile returned %v", err)
	}

	obj, status, err := cat.Lookup(context.Background(), repo, "podinfo")
	if err != nil {
		t.Fatalf("Lookup returned %v", err)
	}
	if obj.GetNamespace() != "team-a" || obj.GetName() != naming.ApplicationChartName("stable", "podinfo") {
		t.Fatalf("Lookup returned %s/%s, want the catalog object next to the repository", obj.GetNamespace(), obj.GetName())
	}
	if len(status.Versions) != 1 || status.Versions[0].Version != "6.7.1" {
		t.Fatalf("status versions = %+v, want [6.7.1]", status.Versions)
	}

	_, _, err = cat.Lookup(context.Background(), repo, "missing")
	if !apierrors.IsNotFound(err) {
		t.Fatalf("Lookup of an unknown chart must be a NotFound error, got %v", err)
	}
}

// TestListErrorNamesAClusterScopedRepositoryWithoutALeadingSlash pins the message a
// failed catalog read puts into the repository's Synced condition: an object key
// renders a cluster-scoped name as "/name", which reaches the user verbatim.
func TestListErrorNamesAClusterScopedRepositoryWithoutALeadingSlash(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := helmv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("registering helm scheme: %v", err)
	}

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
				return errors.New("boom")
			},
		}).
		Build()

	_, err := adapter.NewClusterApplicationCatalog(c).
		Reconcile(context.Background(), adapter.NewClusterApplicationRepository(
			&helmv1alpha1.HelmClusterApplicationRepository{
				ObjectMeta: metav1.ObjectMeta{Name: "shared", UID: types.UID("shared")},
			},
		), nil)
	if err == nil {
		t.Fatal("Reconcile must report the list failure")
	}
	if !strings.Contains(err.Error(), "repository shared:") {
		t.Fatalf("error = %q, want it to name the repository as \"shared\"", err)
	}
}

// TestMigratesALegacyNamedObjectStillInUse pins the migration for a chart a
// consumer still references: the legacy object is deleted regardless of that
// reference, and the version it protected survives on the new object marked
// RemovedFromRepository, media type included, exactly as an ordinary in-use prune
// would have kept it had the name never changed.
func TestMigratesALegacyNamedObjectStillInUse(t *testing.T) {
	const legacyName = "example-chart-podinfo"

	c := newAddonClient(t,
		legacyAddonChart(legacyName, "example", "podinfo",
			helmv1alpha1.ChartVersion{Version: "1.0.0", MediaType: "application/vnd.cncf.helm.chart.content.v1.tar+gzip"},
		),
		addonConsumer("consumer", "example", "podinfo", "1.0.0"),
	)
	cat := adapter.NewAddonCatalog(c)
	repo := addonRepo()

	if err := syncCatalog(cat, repo, chart("podinfo", "2.0.0")); err != nil {
		t.Fatalf("Reconcile returned %v", err)
	}

	newName := naming.HelmClusterAddonChartName("example", "podinfo")

	got := &helmv1alpha1.HelmClusterAddonChart{}
	if err := c.Get(context.Background(), client.ObjectKey{Name: newName}, got); err != nil {
		t.Fatalf("new-scheme object was not created: %v", err)
	}

	var retained *helmv1alpha1.ChartVersion
	for i := range got.Status.Versions {
		if got.Status.Versions[i].Version == "1.0.0" {
			retained = &got.Status.Versions[i]
		}
	}
	if retained == nil {
		t.Fatalf("versions = %+v, want 1.0.0 retained from the legacy object", got.Status.Versions)
	}
	if retained.UnavailableReason != helmv1alpha1.UnavailableReasonRemovedFromRepository {
		t.Fatalf("1.0.0 unavailable reason = %q, want %q", retained.UnavailableReason, helmv1alpha1.UnavailableReasonRemovedFromRepository)
	}
	if retained.MediaType != "application/vnd.cncf.helm.chart.content.v1.tar+gzip" {
		t.Fatalf("1.0.0 media type = %q, want it carried from the legacy object", retained.MediaType)
	}

	err := c.Get(context.Background(), client.ObjectKey{Name: legacyName}, &helmv1alpha1.HelmClusterAddonChart{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("legacy object err = %v, want NotFound: it must be deleted even though a consumer still uses one of its versions", err)
	}
}

// TestMigratesALegacyNamedObjectNotInUse pins the same migration for a chart
// nothing references: the legacy object is still replaced by a new-scheme one, not
// merely left behind the way an ordinary rename would have left it.
func TestMigratesALegacyNamedObjectNotInUse(t *testing.T) {
	const legacyName = "example-chart-podinfo"

	c := newAddonClient(t,
		legacyAddonChart(legacyName, "example", "podinfo",
			helmv1alpha1.ChartVersion{Version: "1.0.0"},
		),
	)
	cat := adapter.NewAddonCatalog(c)
	repo := addonRepo()

	if err := syncCatalog(cat, repo, chart("podinfo", "2.0.0")); err != nil {
		t.Fatalf("Reconcile returned %v", err)
	}

	newName := naming.HelmClusterAddonChartName("example", "podinfo")

	got := &helmv1alpha1.HelmClusterAddonChart{}
	if err := c.Get(context.Background(), client.ObjectKey{Name: newName}, got); err != nil {
		t.Fatalf("new-scheme object was not created: %v", err)
	}
	if len(got.Status.Versions) != 1 || got.Status.Versions[0].Version != "2.0.0" {
		t.Fatalf("versions = %+v, want only the fetched 2.0.0: nothing protects the unreferenced legacy version", got.Status.Versions)
	}

	err := c.Get(context.Background(), client.ObjectKey{Name: legacyName}, &helmv1alpha1.HelmClusterAddonChart{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("legacy object err = %v, want NotFound: it must not be left behind", err)
	}
}

// TestMigrationIsANoOpOnASecondReconcile pins that the seed-and-delete migration
// runs exactly once: the second reconcile finds no legacy object left to seed from
// or delete, so the new object's own status stands and nothing is deleted.
func TestMigrationIsANoOpOnASecondReconcile(t *testing.T) {
	const legacyName = "example-chart-podinfo"

	c := newAddonClient(t,
		legacyAddonChart(legacyName, "example", "podinfo",
			helmv1alpha1.ChartVersion{Version: "1.0.0", MediaType: "application/vnd.cncf.helm.chart.content.v1.tar+gzip"},
		),
		addonConsumer("consumer", "example", "podinfo", "1.0.0"),
	)
	cat := adapter.NewAddonCatalog(c)
	repo := addonRepo()

	if err := syncCatalog(cat, repo, chart("podinfo", "2.0.0")); err != nil {
		t.Fatalf("first Reconcile returned %v", err)
	}

	newName := naming.HelmClusterAddonChartName("example", "podinfo")

	before := &helmv1alpha1.HelmClusterAddonChart{}
	if err := c.Get(context.Background(), client.ObjectKey{Name: newName}, before); err != nil {
		t.Fatalf("getting the migrated object: %v", err)
	}
	if err := c.Get(context.Background(), client.ObjectKey{Name: legacyName}, &helmv1alpha1.HelmClusterAddonChart{}); !apierrors.IsNotFound(err) {
		t.Fatalf("legacy object err = %v, want NotFound after the first reconcile already migrated it", err)
	}

	deletes := 0
	c = interceptedClient(t, c, func() { deletes++ })

	cat = adapter.NewAddonCatalog(c)
	if err := syncCatalog(cat, repo, chart("podinfo", "2.0.0")); err != nil {
		t.Fatalf("second Reconcile returned %v", err)
	}

	if deletes != 0 {
		t.Fatalf("second Reconcile issued %d delete(s), want none: nothing is left over to migrate", deletes)
	}

	after := &helmv1alpha1.HelmClusterAddonChart{}
	if err := c.Get(context.Background(), client.ObjectKey{Name: newName}, after); err != nil {
		t.Fatalf("getting the object after the second reconcile: %v", err)
	}
	if len(after.Status.Versions) != len(before.Status.Versions) || after.Status.Versions[0] != before.Status.Versions[0] {
		t.Fatalf("status changed on a no-op reconcile: before %+v, after %+v", before.Status.Versions, after.Status.Versions)
	}
}

func interceptedClient(t *testing.T, c client.WithWatch, onDelete func()) client.WithWatch {
	t.Helper()

	return interceptor.NewClient(c, interceptor.Funcs{
		Delete: func(ctx context.Context, wc client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			onDelete()

			return wc.Delete(ctx, obj, opts...)
		},
	})
}

// TestMigrationLeavesUnrelatedChartsToExistingPruning pins that the migration only
// touches charts being reconciled: an unrelated chart's object is still governed by
// the ordinary in-use rule, kept while referenced and pruned once it is not.
func TestMigrationLeavesUnrelatedChartsToExistingPruning(t *testing.T) {
	c := newAddonClient(t,
		&helmv1alpha1.HelmClusterAddonChart{
			ObjectMeta: metav1.ObjectMeta{
				Name:   naming.HelmClusterAddonChartName("example", "kept"),
				Labels: map[string]string{helmv1alpha1.LabelRepositoryName: "example", helmv1alpha1.LabelChartName: "kept"},
			},
		},
		&helmv1alpha1.HelmClusterAddonChart{
			ObjectMeta: metav1.ObjectMeta{
				Name:   naming.HelmClusterAddonChartName("example", "pruned"),
				Labels: map[string]string{helmv1alpha1.LabelRepositoryName: "example", helmv1alpha1.LabelChartName: "pruned"},
			},
		},
		addonConsumer("consumer", "example", "kept", "1.0.0"),
	)
	cat := adapter.NewAddonCatalog(c)
	repo := addonRepo()

	// podinfo is the only chart being reconciled; "kept" and "pruned" are not part
	// of this call, exactly like an ordinary reconcile of a repository whose index
	// dropped them.
	if err := syncCatalog(cat, repo, chart("podinfo", "1.0.0")); err != nil {
		t.Fatalf("Reconcile returned %v", err)
	}

	err := c.Get(context.Background(), client.ObjectKey{Name: naming.HelmClusterAddonChartName("example", "kept")}, &helmv1alpha1.HelmClusterAddonChart{})
	if err != nil {
		t.Fatalf("the referenced unrelated chart must be kept, got %v", err)
	}

	err = c.Get(context.Background(), client.ObjectKey{Name: naming.HelmClusterAddonChartName("example", "pruned")}, &helmv1alpha1.HelmClusterAddonChart{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("the unreferenced unrelated chart must be pruned, got %v", err)
	}
}

// TestMigratesAChartTheRepositoryNoLongerOffers pins the case the pruning loop
// cannot handle: the repository dropped the chart entirely and only a consumer keeps
// it alive. Its object is not part of any fetch, so a migration driven by the fetched
// charts would leave it under the legacy name, while the consumer already resolves
// the new one and its release would never reconcile again.
func TestMigratesAChartTheRepositoryNoLongerOffers(t *testing.T) {
	const legacyName = "example-chart-gone"

	c := newAddonClient(t,
		legacyAddonChart(legacyName, "example", "gone",
			helmv1alpha1.ChartVersion{Version: "1.0.0", MediaType: "application/vnd.cncf.helm.chart.content.v1.tar+gzip"},
		),
		addonConsumer("consumer", "example", "gone", "1.0.0"),
	)
	cat := adapter.NewAddonCatalog(c)

	if err := syncCatalog(cat, addonRepo(), chart("podinfo", "2.0.0")); err != nil {
		t.Fatalf("Reconcile returned %v", err)
	}

	got := &helmv1alpha1.HelmClusterAddonChart{}
	if err := c.Get(context.Background(), client.ObjectKey{Name: naming.HelmClusterAddonChartName("example", "gone")}, got); err != nil {
		t.Fatalf("the chart a consumer still holds was not moved to its new name: %v", err)
	}
	if len(got.Status.Versions) != 1 || got.Status.Versions[0].Version != "1.0.0" {
		t.Fatalf("versions = %+v, want the legacy status carried over", got.Status.Versions)
	}
	if got.Status.Versions[0].MediaType == "" {
		t.Fatal("the media type was lost, so the consumer's internal source cannot be built")
	}

	err := c.Get(context.Background(), client.ObjectKey{Name: legacyName}, &helmv1alpha1.HelmClusterAddonChart{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("legacy object err = %v, want NotFound", err)
	}
}

// TestMigrationSurvivesAFailedStatusWrite pins that the legacy object outlives a
// failure: it is the only copy of a retained version, so deleting it before its
// status has landed would lose that version for good.
func TestMigrationSurvivesAFailedStatusWrite(t *testing.T) {
	const legacyName = "example-chart-podinfo"

	failed := false
	c := newAddonClientWithInterceptor(t, interceptor.Funcs{
		SubResourcePatch: func(ctx context.Context, cl client.Client, sub string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
			if !failed {
				failed = true

				return errors.New("transient status patch failure")
			}

			return cl.SubResource(sub).Patch(ctx, obj, patch, opts...)
		},
	},
		legacyAddonChart(legacyName, "example", "podinfo",
			helmv1alpha1.ChartVersion{Version: "1.0.0", MediaType: "application/vnd.cncf.helm.chart.content.v1.tar+gzip"},
		),
		addonConsumer("consumer", "example", "podinfo", "1.0.0"),
	)
	cat := adapter.NewAddonCatalog(c)

	if err := syncCatalog(cat, addonRepo(), chart("podinfo", "2.0.0")); err == nil {
		t.Fatal("Reconcile must report the failed status write")
	}

	if err := c.Get(context.Background(), client.ObjectKey{Name: legacyName}, &helmv1alpha1.HelmClusterAddonChart{}); err != nil {
		t.Fatalf("legacy object err = %v, want it kept until its status has been carried over", err)
	}

	if err := syncCatalog(cat, addonRepo(), chart("podinfo", "2.0.0")); err != nil {
		t.Fatalf("second Reconcile returned %v", err)
	}

	got := &helmv1alpha1.HelmClusterAddonChart{}
	if err := c.Get(context.Background(), client.ObjectKey{Name: naming.HelmClusterAddonChartName("example", "podinfo")}, got); err != nil {
		t.Fatalf("new-scheme object was not created: %v", err)
	}

	var retained bool
	for _, version := range got.Status.Versions {
		if version.Version == "1.0.0" && version.MediaType != "" {
			retained = true
		}
	}
	if !retained {
		t.Fatalf("versions = %+v, want 1.0.0 and its media type carried over on the retry", got.Status.Versions)
	}
}

// rejectInvalidLabels makes the fake client behave like the API server on the one
// rule this test turns on: a label value outside the label grammar is rejected. The
// fake client accepts any string, so without this the failure under test cannot
// happen at all.
func rejectInvalidLabels() interceptor.Funcs {
	check := func(obj client.Object) error {
		for key, value := range obj.GetLabels() {
			if errs := validation.IsValidLabelValue(value); len(errs) > 0 {
				return apierrors.NewInvalid(
					obj.GetObjectKind().GroupVersionKind().GroupKind(), obj.GetName(),
					field.ErrorList{field.Invalid(field.NewPath("metadata", "labels", key), value, strings.Join(errs, "; "))},
				)
			}
		}

		return nil
	}

	return interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if err := check(obj); err != nil {
				return err
			}

			return c.Create(ctx, obj, opts...)
		},
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			if err := check(obj); err != nil {
				return err
			}

			return c.Patch(ctx, obj, patch, opts...)
		},
	}
}

// TestReconcileSkipsAChartWhoseNameCannotBeALabel pins that one chart the catalog
// cannot record does not cost the whole repository its synchronization. The chart
// name is copied verbatim into a label value, and an index may legally publish a
// name no label value can hold; the write is rejected, and a pass that returned
// there would leave every chart listed after it unwritten and never reach the
// pruning loop at all — so a chart removed from the repository would stay in the
// catalog forever and the repository would never report a successful sync.
func TestReconcileSkipsAChartWhoseNameCannotBeALabel(t *testing.T) {
	stale := legacyAddonChart(naming.HelmClusterAddonChartName("example", "gone"), "example", "gone",
		helmv1alpha1.ChartVersion{Version: "1.0.0"})

	c := newAddonClientWithInterceptor(t, rejectInvalidLabels(), stale)
	cat := adapter.NewAddonCatalog(c)
	repo := addonRepo()

	// The unwritable chart comes first, so a pass that stopped on it would also be
	// missing the one after it.
	result, err := cat.Reconcile(context.Background(), repo, []repoclient.Chart{
		chart("my chart", "1.0.0"),
		chart("podinfo", "6.7.1"),
	})
	if err != nil {
		t.Fatalf("Reconcile returned %v, want the pass to complete", err)
	}
	if !slices.Equal(result.SkippedCharts, []string{"my chart"}) {
		t.Fatalf("skipped charts are %q, want the pass to report the chart it left out", result.SkippedCharts)
	}

	good := &helmv1alpha1.HelmClusterAddonChart{}
	if err := c.Get(context.Background(), client.ObjectKey{Name: naming.HelmClusterAddonChartName("example", "podinfo")}, good); err != nil {
		t.Fatalf("the chart listed after the unwritable one was not created: %v", err)
	}

	skipped := &helmv1alpha1.HelmClusterAddonChart{}
	err = c.Get(context.Background(), client.ObjectKey{Name: naming.HelmClusterAddonChartName("example", "my chart")}, skipped)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("getting the skipped chart returned %v, want NotFound", err)
	}

	pruned := &helmv1alpha1.HelmClusterAddonChart{}
	err = c.Get(context.Background(), client.ObjectKey{Name: stale.Name}, pruned)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("the stale chart was not pruned (%v), so the pass never reached the pruning loop", err)
	}
}

// TestReconcileReportsEveryChartItCannotPublish pins which chart names cost an
// object and that each one is handed back to the caller. The repository status is
// derived from this list: it counts the charts the cluster actually shows and names
// the rest, so a chart dropped here without being reported would leave a user with a
// count that does not match the objects and nothing to explain the difference.
func TestReconcileReportsEveryChartItCannotPublish(t *testing.T) {
	// A label value is limited to 63 characters. The object name is not: it is
	// truncated and hashed, so length alone never decides. The pair below is what
	// pins that the cut-off is the label rule and nothing else.
	longEnough := strings.Repeat("a", 63)
	tooLong := strings.Repeat("a", 64)

	cases := []struct {
		name     string
		charts   []string
		wantKept []string
		wantSkip []string
	}{
		{
			name:     "a name a label value can hold is published",
			charts:   []string{"podinfo", longEnough},
			wantKept: []string{"podinfo", longEnough},
		},
		{
			name:     "a name longer than a label value is reported, not dropped",
			charts:   []string{tooLong},
			wantSkip: []string{tooLong},
		},
		{
			name:     "a name outside the label grammar is reported, not dropped",
			charts:   []string{"my chart", "_internal", "chart/with/slashes"},
			wantSkip: []string{"my chart", "_internal", "chart/with/slashes"},
		},
		{
			// The two groups are separated, not reordered: the skipped names come back
			// in the order they were handed over. That order carries no meaning of its
			// own — a Helm index is a map, so the caller sees a different one each read
			// — which is why the status message sorts before naming anything.
			name:     "publishable and unpublishable names are separated",
			charts:   []string{"my chart", "podinfo", tooLong, "nginx"},
			wantKept: []string{"podinfo", "nginx"},
			wantSkip: []string{"my chart", tooLong},
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			c := newAddonClientWithInterceptor(t, rejectInvalidLabels())
			repo := addonRepo()

			charts := make([]repoclient.Chart, 0, len(tt.charts))
			for _, name := range tt.charts {
				charts = append(charts, chart(name, "1.0.0"))
			}

			result, err := adapter.NewAddonCatalog(c).Reconcile(context.Background(), repo, charts)
			if err != nil {
				t.Fatalf("Reconcile returned %v, want the pass to complete", err)
			}

			if !slices.Equal(result.SkippedCharts, tt.wantSkip) {
				t.Fatalf("skipped charts are %q, want %q", result.SkippedCharts, tt.wantSkip)
			}

			for _, name := range tt.wantKept {
				objectName := naming.HelmClusterAddonChartName("example", name)
				if errs := validation.IsDNS1123Subdomain(objectName); len(errs) > 0 {
					t.Fatalf("the name derived for chart %q is not a valid object name: %s", name, strings.Join(errs, "; "))
				}
				if err := c.Get(context.Background(), client.ObjectKey{Name: objectName}, &helmv1alpha1.HelmClusterAddonChart{}); err != nil {
					t.Fatalf("chart %q was not published: %v", name, err)
				}
			}

			published := &helmv1alpha1.HelmClusterAddonChartList{}
			if err := c.List(context.Background(), published); err != nil {
				t.Fatalf("listing published charts: %v", err)
			}
			// The count the repository status reports is len(charts) minus the skipped
			// ones, so it is only checkable if that is exactly what the catalog holds.
			if len(published.Items) != len(tt.charts)-len(tt.wantSkip) {
				t.Fatalf("catalog holds %d objects, want %d", len(published.Items), len(tt.charts)-len(tt.wantSkip))
			}
		})
	}
}

// TestAFailedProbeOnAVersionInUseIsExaminedAgainOnTheNextPass closes the loop that
// left a transiently unreachable version pending forever: the merge carries the
// recorded media type onto the failed probe of a version still in use (without it the
// running application's internal OCIRepository could not be built), and the next pass
// reads that same catalog back as its known state. If a carried media type counted as
// an examined tag, the tag would never be probed again, its stale ResolvePending would
// be republished every pass, and the repository would never report a successful sync
// again. The known state here is built by the catalog itself, the way the
// synchronizer builds it, so the assertion is about production's own state and not
// about a hand-written map.
func TestAFailedProbeOnAVersionInUseIsExaminedAgainOnTheNextPass(t *testing.T) {
	const mediaType = "application/vnd.cncf.helm.chart.content.v1.tar+gzip"

	c := newClient(t)
	cat := adapter.NewApplicationCatalog(c)
	repo := applicationRepo("team-a", "stable")

	app := &helmv1alpha1.HelmApplication{
		ObjectMeta: metav1.ObjectMeta{Name: "podinfo", Namespace: "team-a"},
		Spec: helmv1alpha1.HelmApplicationSpec{
			Chart: helmv1alpha1.HelmApplicationChartRef{Name: "podinfo", Repository: "stable", Version: "6.7.1"},
		},
	}
	if err := c.Create(context.Background(), app); err != nil {
		t.Fatalf("creating the application using the version: %v", err)
	}

	resolved := repoclient.Chart{Name: "podinfo", Versions: []repoclient.ChartVersion{
		{Version: semver.MustParse("6.7.1"), MediaType: mediaType},
	}}
	if _, err := cat.Reconcile(context.Background(), repo, []repoclient.Chart{resolved}); err != nil {
		t.Fatalf("the resolving pass returned %v", err)
	}

	// The next pass cannot reach the registry for that tag: no media type, no
	// reference, only the transport failure.
	pending := repoclient.Chart{Name: "podinfo", Versions: []repoclient.ChartVersion{{
		Version:            semver.MustParse("6.7.1"),
		UnavailableReason:  helmv1alpha1.UnavailableReasonResolvePending,
		UnavailableMessage: "GET https://registry.example.invalid/v2/: dial tcp: i/o timeout",
	}}}
	if _, err := cat.Reconcile(context.Background(), repo, []repoclient.Chart{pending}); err != nil {
		t.Fatalf("the failing pass returned %v", err)
	}

	stored := &helmv1alpha1.HelmApplicationChart{}
	key := client.ObjectKey{Namespace: "team-a", Name: naming.ApplicationChartName("stable", "podinfo")}
	if err := c.Get(context.Background(), key, stored); err != nil {
		t.Fatalf("reading the catalog object: %v", err)
	}
	if len(stored.Status.Versions) != 1 {
		t.Fatalf("versions = %+v, want exactly one", stored.Status.Versions)
	}
	if got := stored.Status.Versions[0]; got.MediaType != mediaType ||
		got.UnavailableReason != helmv1alpha1.UnavailableReasonResolvePending {
		t.Fatalf("recorded version = %+v, want the carried media type next to the pending reason", got)
	}

	known, err := cat.Known(context.Background(), repo)
	if err != nil {
		t.Fatalf("Known returned %v", err)
	}

	opts := repoclient.FetchOptions{Known: known, Full: repo.ForceReconcileRequired()}
	if !opts.NeedsExamination("podinfo", "6.7.1") {
		t.Fatal("a version left pending by a failed probe must be examined again on the next pass")
	}

	// Being examined again is only half of it: the pass that reaches the registry has
	// to leave nothing of the failure behind, or the repository would keep counting a
	// pending version and never report a successful sync.
	if _, err := cat.Reconcile(context.Background(), repo, []repoclient.Chart{resolved}); err != nil {
		t.Fatalf("the recovering pass returned %v", err)
	}

	recovered := &helmv1alpha1.HelmApplicationChart{}
	if err := c.Get(context.Background(), key, recovered); err != nil {
		t.Fatalf("reading the catalog object after recovery: %v", err)
	}
	if len(recovered.Status.Versions) != 1 {
		t.Fatalf("versions after recovery = %+v, want exactly one", recovered.Status.Versions)
	}
	if got := recovered.Status.Versions[0]; got.MediaType != mediaType ||
		got.UnavailableReason != "" || got.UnavailableMessage != "" {
		t.Fatalf("recovered version = %+v, want the media type alone with nothing of the failure left", got)
	}
}

// TestAnIncompletePassKeepsTheVersionsItDidNotExamine pins what a fetch that ran out
// of its own budget may and may not change. The versions it examined are written, and
// the ones it never got to keep exactly the verdict a previous pass recorded: not
// dropped, which would cost the next pass the work this one already paid for, and not
// marked removed from the repository, which is a statement about the remote that this
// pass made no observation to support.
func TestAnIncompletePassKeepsTheVersionsItDidNotExamine(t *testing.T) {
	const mediaType = "application/vnd.cncf.helm.chart.content.v1.tar+gzip"

	c := newClient(t)
	cat := adapter.NewApplicationCatalog(c)
	repo := applicationRepo("team-a", "stable")

	complete := repoclient.Chart{Name: "podinfo", Versions: []repoclient.ChartVersion{
		{Version: semver.MustParse("6.7.1"), MediaType: mediaType},
		{Version: semver.MustParse("6.7.0"), MediaType: mediaType},
	}}
	if _, err := cat.Reconcile(context.Background(), repo, []repoclient.Chart{complete}); err != nil {
		t.Fatalf("the first pass returned %v", err)
	}

	// The next pass carries 6.7.0 from the known state, examines the new 6.8.0 and
	// runs out of budget before reaching 6.7.1.
	incomplete := repoclient.Chart{
		Name: "podinfo",
		Versions: []repoclient.ChartVersion{
			{Version: semver.MustParse("6.8.0"), MediaType: mediaType},
			{Version: semver.MustParse("6.7.0"), MediaType: mediaType},
		},
		UnexaminedVersions: []string{"6.7.1"},
	}
	if _, err := cat.Reconcile(context.Background(), repo, []repoclient.Chart{incomplete}); err != nil {
		t.Fatalf("the incomplete pass returned %v", err)
	}

	stored := &helmv1alpha1.HelmApplicationChart{}
	key := client.ObjectKey{Namespace: "team-a", Name: naming.ApplicationChartName("stable", "podinfo")}
	if err := c.Get(context.Background(), key, stored); err != nil {
		t.Fatalf("reading the catalog object: %v", err)
	}

	versions := map[string]helmv1alpha1.ChartVersion{}
	for _, version := range stored.Status.Versions {
		versions[version.Version] = version
	}

	if len(versions) != 3 {
		t.Fatalf("versions = %+v, want the two carried ones plus the new one", stored.Status.Versions)
	}
	if got := versions["6.8.0"].MediaType; got != mediaType {
		t.Fatalf("6.8.0 media type = %q, want the one this pass examined", got)
	}

	deferred, recorded := versions["6.7.1"]
	if !recorded {
		t.Fatal("a version this pass did not examine must keep its recorded entry")
	}
	if deferred.MediaType != mediaType {
		t.Fatalf("6.7.1 media type = %q, want the recorded one", deferred.MediaType)
	}
	if deferred.UnavailableReason != "" {
		t.Fatalf("6.7.1 reason = %q: an unexamined version must not be given a verdict", deferred.UnavailableReason)
	}

	// The point of keeping it: the next pass skips it, which is what lets a repository
	// larger than one budget converge over several passes.
	known, err := cat.Known(context.Background(), repo)
	if err != nil {
		t.Fatalf("Known returned %v", err)
	}
	if (repoclient.FetchOptions{Known: known}).NeedsExamination("podinfo", "6.7.1") {
		t.Fatal("a version kept with its verdict must not be examined again")
	}
}

// TestAnIncompletePassPrunesNothing pins the most destructive thing a partially read
// repository could do. Pruning reads "the remote did not list it" out of "it is not in
// what we were handed", and a pass that stopped early is in no position to say that.
// Deleting a chart that exists remotely costs every consumer of it its reconciliation,
// so the pruning waits for a pass that saw the whole repository.
func TestAnIncompletePassPrunesNothing(t *testing.T) {
	c := newClient(t)
	cat := adapter.NewApplicationCatalog(c)
	repo := applicationRepo("team-a", "stable")

	if _, err := cat.Reconcile(context.Background(), repo,
		[]repoclient.Chart{chart("podinfo", "1.0.0"), chart("nginx", "1.0.0")}); err != nil {
		t.Fatalf("the first pass returned %v", err)
	}

	incomplete := chart("podinfo", "1.0.0")
	incomplete.UnexaminedVersions = []string{"1.0.1"}

	if _, err := cat.Reconcile(context.Background(), repo, []repoclient.Chart{incomplete}); err != nil {
		t.Fatalf("the incomplete pass returned %v", err)
	}

	var charts helmv1alpha1.HelmApplicationChartList
	if err := c.List(context.Background(), &charts, client.InNamespace("team-a")); err != nil {
		t.Fatalf("listing charts: %v", err)
	}
	if len(charts.Items) != 2 {
		t.Fatalf("charts = %v, want both: an incomplete pass may delete nothing", charts.Items)
	}

	// The guard is the incompleteness, not a change of heart about pruning: the next
	// complete pass still removes the chart the repository no longer offers.
	if _, err := cat.Reconcile(context.Background(), repo, []repoclient.Chart{chart("podinfo", "1.0.0")}); err != nil {
		t.Fatalf("the complete pass returned %v", err)
	}
	if err := c.List(context.Background(), &charts, client.InNamespace("team-a")); err != nil {
		t.Fatalf("listing charts: %v", err)
	}
	if len(charts.Items) != 1 || charts.Items[0].Labels[helmv1alpha1.LabelChartName] != "podinfo" {
		t.Fatalf("charts = %v, want only podinfo after a complete pass", charts.Items)
	}
}
