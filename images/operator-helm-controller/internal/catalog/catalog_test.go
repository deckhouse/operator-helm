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
	"strings"
	"testing"

	"github.com/Masterminds/semver/v3"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/deckhouse/operator-helm/api/naming"
	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
	"github.com/deckhouse/operator-helm/internal/adapter"
	repoclient "github.com/deckhouse/operator-helm/internal/client/repository"
	"github.com/deckhouse/operator-helm/internal/index"
	"github.com/deckhouse/operator-helm/internal/source"
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

func applicationRepo(namespace, name string) source.Repository {
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

func addonRepo() source.Repository {
	return adapter.NewAddonRepository(&helmv1alpha1.HelmClusterAddonRepository{ObjectMeta: metav1.ObjectMeta{Name: "example"}})
}

// addonConsumer builds a HelmClusterAddon referencing one repository/chart/version,
// so InUseVersions reports that version as in use.
func addonConsumer(name, repoName, chartName, version string) *helmv1alpha1.HelmClusterAddon {
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
func legacyAddonChart(name, repoName, chartName string, versions ...helmv1alpha1.ChartVersion) *helmv1alpha1.HelmClusterAddonChart {
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

	if err := cat.Reconcile(context.Background(), repo, []repoclient.Chart{chart("podinfo", "6.7.1")}); err != nil {
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

	if err := cat.Reconcile(context.Background(), teamA, []repoclient.Chart{chart("podinfo", "1.0.0")}); err != nil {
		t.Fatalf("Reconcile(team-a) returned %v", err)
	}
	if err := cat.Reconcile(context.Background(), teamB, []repoclient.Chart{chart("nginx", "2.0.0")}); err != nil {
		t.Fatalf("Reconcile(team-b) returned %v", err)
	}

	known, err := cat.Known(context.Background(), teamA)
	if err != nil {
		t.Fatalf("Known returned %v", err)
	}
	if _, ok := known["podinfo"]; !ok || len(known) != 1 {
		t.Fatalf("Known(team-a) = %v, want only podinfo", known)
	}

	// Re-reconciling team-a with an empty list prunes its own catalog only.
	if err := cat.Reconcile(context.Background(), teamA, nil); err != nil {
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

	if err := cat.Reconcile(context.Background(), repo, []repoclient.Chart{chart("podinfo", "6.7.1")}); err != nil {
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

	if err := cat.Reconcile(context.Background(), repo, []repoclient.Chart{chart("podinfo", "1.0.0"), chart("nginx", "1.0.0")}); err != nil {
		t.Fatalf("first Reconcile returned %v", err)
	}
	if err := cat.Reconcile(context.Background(), repo, []repoclient.Chart{chart("podinfo", "1.0.0")}); err != nil {
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

	if err := cat.Reconcile(context.Background(), repo, []repoclient.Chart{chart("podinfo", "6.7.1")}); err != nil {
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

	err := adapter.NewClusterApplicationCatalog(c).
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

	if err := cat.Reconcile(context.Background(), repo, []repoclient.Chart{chart("podinfo", "2.0.0")}); err != nil {
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

	if err := cat.Reconcile(context.Background(), repo, []repoclient.Chart{chart("podinfo", "2.0.0")}); err != nil {
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

	if err := cat.Reconcile(context.Background(), repo, []repoclient.Chart{chart("podinfo", "2.0.0")}); err != nil {
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
	if err := cat.Reconcile(context.Background(), repo, []repoclient.Chart{chart("podinfo", "2.0.0")}); err != nil {
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

// interceptedClient wraps c so onDelete is called for every delete it forwards,
// while every other call still reaches c unchanged.
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
	if err := cat.Reconcile(context.Background(), repo, []repoclient.Chart{chart("podinfo", "1.0.0")}); err != nil {
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

	if err := cat.Reconcile(context.Background(), addonRepo(), []repoclient.Chart{chart("podinfo", "2.0.0")}); err != nil {
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

	if err := cat.Reconcile(context.Background(), addonRepo(), []repoclient.Chart{chart("podinfo", "2.0.0")}); err == nil {
		t.Fatal("Reconcile must report the failed status write")
	}

	if err := c.Get(context.Background(), client.ObjectKey{Name: legacyName}, &helmv1alpha1.HelmClusterAddonChart{}); err != nil {
		t.Fatalf("legacy object err = %v, want it kept until its status has been carried over", err)
	}

	if err := cat.Reconcile(context.Background(), addonRepo(), []repoclient.Chart{chart("podinfo", "2.0.0")}); err != nil {
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
