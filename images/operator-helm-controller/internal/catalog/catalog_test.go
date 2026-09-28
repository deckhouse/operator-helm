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

	return newClientWithInterceptor(t, interceptor.Funcs{})
}

func newClientWithInterceptor(t *testing.T, funcs interceptor.Funcs) client.Client {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := helmv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("registering helm scheme: %v", err)
	}

	return fake.NewClientBuilder().
		WithScheme(scheme).
		WithInterceptorFuncs(funcs).
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

	_, err := reconcileOnce(cat, repo, charts)

	return err
}

// reconcileOnce runs one whole pass that marks nothing: the single authoritative read
// the production path takes at the start of a synchronization, and the write computed
// from it. Nothing on that path reads the catalog twice, so no test may either — a test
// that opened a fresh snapshot for the write would model a controller this is not.
func reconcileOnce(cat contract.Catalog, repo contract.Repository, charts []repoclient.Chart) (contract.CatalogResult, error) {
	snapshot, err := cat.Snapshot(context.Background(), repo)
	if err != nil {
		return contract.CatalogResult{}, err
	}

	return snapshot.Reconcile(context.Background(), charts)
}

// knownCharts reads the verdicts a pass would skip tags by.
func knownCharts(cat contract.Catalog, repo contract.Repository) (repoclient.KnownCharts, error) {
	snapshot, err := cat.Snapshot(context.Background(), repo)
	if err != nil {
		return nil, err
	}

	return snapshot.Known(), nil
}

// forcedPass opens the pass a forced refresh runs: it records the request on every
// recorded verdict and hands back the snapshot the rest of that pass — the fetch it
// decides from and the write it publishes — is computed from.
func forcedPass(t *testing.T, cat contract.Catalog, repo contract.Repository) contract.CatalogSnapshot {
	t.Helper()

	snapshot, err := cat.Snapshot(context.Background(), repo)
	if err != nil {
		t.Fatalf("reading the catalog returned %v", err)
	}

	if err := snapshot.MarkForReexamination(context.Background()); err != nil {
		t.Fatalf("marking the recorded versions returned %v", err)
	}

	return snapshot
}

func chart(name, version string) repoclient.Chart {
	return repoclient.Chart{
		Name:     name,
		Versions: []repoclient.ChartVersion{{Version: semver.MustParse(version), IconURL: "https://example.invalid/" + name + ".png"}},
	}
}

func TestReconcileWritesNamespacedCatalogNextToTheRepository(t *testing.T) {
	c := newClient(t)
	cat := adapter.NewApplicationCatalog(c, c)
	repo := applicationRepo("team-a", "stable")

	if _, err := reconcileOnce(cat, repo, []repoclient.Chart{chart("podinfo", "6.7.1")}); err != nil {
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

// TestReconcileRepairsAnObjectStrippedOfItsLabels covers the catalog object a pass
// cannot see. The catalog is listed by the repository label, so an object that lost its
// labels is absent from every read this pass takes while still occupying the name the
// chart is published under. This pass may not write its status — it was computed
// without knowing the object existed — but the labels have to be repaired, or the
// object is missing from every later snapshot too and the collision is permanent
// instead of a lost race.
func TestReconcileRepairsAnObjectStrippedOfItsLabels(t *testing.T) {
	c := newClient(t)
	cat := adapter.NewApplicationCatalog(c, c)
	repo := applicationRepo("team-a", "stable")
	key := client.ObjectKey{Namespace: "team-a", Name: naming.ApplicationChartName("stable", "podinfo")}

	stripped := &helmv1alpha1.HelmApplicationChart{
		ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace},
	}
	if err := c.Create(context.Background(), stripped); err != nil {
		t.Fatalf("creating the unlabelled object: %v", err)
	}

	_, err := reconcileOnce(cat, repo, []repoclient.Chart{chart("podinfo", "6.7.1")})
	if !apierrors.IsConflict(err) {
		t.Fatalf("Reconcile returned %v, want a conflict: the object was not in the read this pass was computed from", err)
	}

	got := &helmv1alpha1.HelmApplicationChart{}
	if err := c.Get(context.Background(), key, got); err != nil {
		t.Fatalf("reading the repaired object: %v", err)
	}
	if got.Labels[helmv1alpha1.LabelRepositoryName] != "stable" || got.Labels[helmv1alpha1.LabelChartName] != "podinfo" {
		t.Fatalf("labels = %v, want the object labelled back into the catalog", got.Labels)
	}
	if len(got.OwnerReferences) != 1 || got.OwnerReferences[0].Name != "stable" {
		t.Fatalf("owner references = %v, want the repository owning it again", got.OwnerReferences)
	}

	// The retry the work queue runs: the object is labelled, so it is part of this
	// read, and the chart is published onto it like onto any other recorded object.
	if _, err := reconcileOnce(cat, repo, []repoclient.Chart{chart("podinfo", "6.7.1")}); err != nil {
		t.Fatalf("the pass after the repair returned %v", err)
	}

	if err := c.Get(context.Background(), key, got); err != nil {
		t.Fatalf("reading the object after the retry: %v", err)
	}
	if len(got.Status.Versions) != 1 || got.Status.Versions[0].Version != "6.7.1" {
		t.Fatalf("versions = %+v, want the chart published onto the repaired object", got.Status.Versions)
	}
}

// TestReconcileRejectsPublishingOntoAnObjectCreatedAfterTheRead covers the other way a
// chart can be missing from a pass's read: it was genuinely not in the catalog when the
// pass started, and another writer created it while this pass was probing the registry.
// Between the two, the tag was pushed again, so the object records a media type this
// pass's own probe — older than the other writer's — knows nothing about.
//
// Nothing catches this downstream. The create fails, and a merge into the object just
// read would be patched against the current resource version, so the optimistic lock
// accepts the write and the catalog publishes the superseded media type; every later
// pass then skips the version, a recorded unmarked verdict being exactly what a pass
// carries forward. The absence from the read is itself the evidence that the pass is
// behind, so the only safe answer is to write no status and report the race.
func TestReconcileRejectsPublishingOntoAnObjectCreatedAfterTheRead(t *testing.T) {
	const (
		probed   = "application/tar+gzip"
		repushed = "application/vnd.cncf.helm.chart.content.v1.tar+gzip"
	)

	key := client.ObjectKey{Namespace: "team-a", Name: naming.ApplicationChartName("stable", "podinfo")}

	// The competing writer lands between this pass's read and its create: it publishes
	// the chart with the media type of the re-pushed artifact and asks, on an annotation
	// already consumed, for that verdict to be looked at again.
	overtaken := false
	c := newClientWithInterceptor(t, interceptor.Funcs{
		Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if obj.GetName() == key.Name && !overtaken {
				overtaken = true

				written := &helmv1alpha1.HelmApplicationChart{
					ObjectMeta: metav1.ObjectMeta{
						Name:      key.Name,
						Namespace: key.Namespace,
						Labels: map[string]string{
							helmv1alpha1.LabelDeckhouseHeritage: helmv1alpha1.LabelDeckhouseHeritageValue,
							helmv1alpha1.LabelRepositoryName:    "stable",
							helmv1alpha1.LabelChartName:         "podinfo",
						},
					},
				}
				if err := cl.Create(ctx, written); err != nil {
					return err
				}

				written.Status.Versions = []helmv1alpha1.ChartVersion{
					{Version: "1.0.0", MediaType: repushed, ReexaminationRequired: true},
				}
				if err := cl.Status().Update(ctx, written); err != nil {
					return err
				}
			}

			return cl.Create(ctx, obj, opts...)
		},
	})
	cat := adapter.NewApplicationCatalog(c, c)
	repo := applicationRepo("team-a", "stable")

	fetched := repoclient.Chart{
		Name:     "podinfo",
		Versions: []repoclient.ChartVersion{{Version: semver.MustParse("1.0.0"), MediaType: probed}},
	}

	_, err := reconcileOnce(cat, repo, []repoclient.Chart{fetched})
	if !apierrors.IsConflict(err) {
		t.Fatalf("Reconcile returned %v, want a conflict: the catalog gained the object after this pass read it", err)
	}

	got := &helmv1alpha1.HelmApplicationChart{}
	if err := c.Get(context.Background(), key, got); err != nil {
		t.Fatalf("reading the object the other writer created: %v", err)
	}
	if len(got.Status.Versions) != 1 || got.Status.Versions[0].Version != "1.0.0" {
		t.Fatalf("versions = %+v, want the other writer's single entry", got.Status.Versions)
	}
	if got.Status.Versions[0].MediaType != repushed {
		t.Fatalf("media type = %q, want %q: this pass probed before the re-push and must not publish what it found",
			got.Status.Versions[0].MediaType, repushed)
	}
	if !got.Status.Versions[0].ReexaminationRequired {
		t.Fatal("the mark the other writer recorded must survive: the force asking for it is already consumed")
	}
}

// TestSameNamedRepositoriesKeepSeparateCatalogs is why the namespaced catalog
// lists by namespace: two repositories named "stable" in different namespaces
// produce same-named chart objects, and one repository's pruning must not see the
// other's.
func TestSameNamedRepositoriesKeepSeparateCatalogs(t *testing.T) {
	c := newClient(t)
	cat := adapter.NewApplicationCatalog(c, c)
	teamA := applicationRepo("team-a", "stable")
	teamB := applicationRepo("team-b", "stable")

	if _, err := reconcileOnce(cat, teamA, []repoclient.Chart{chart("podinfo", "1.0.0")}); err != nil {
		t.Fatalf("Reconcile(team-a) returned %v", err)
	}
	if _, err := reconcileOnce(cat, teamB, []repoclient.Chart{chart("nginx", "2.0.0")}); err != nil {
		t.Fatalf("Reconcile(team-b) returned %v", err)
	}

	known, err := knownCharts(cat, teamA)
	if err != nil {
		t.Fatalf("Known returned %v", err)
	}
	if _, ok := known["podinfo"]; !ok || len(known) != 1 {
		t.Fatalf("Known(team-a) = %v, want only podinfo", known)
	}

	if _, err := reconcileOnce(cat, teamA, nil); err != nil {
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
	cat := adapter.NewClusterApplicationCatalog(c, c)
	repo := adapter.NewClusterApplicationRepository(&helmv1alpha1.HelmClusterApplicationRepository{
		ObjectMeta: metav1.ObjectMeta{Name: "shared", UID: "shared"},
		Spec:       helmv1alpha1.RepositorySpec{URL: "oci://ghcr.io/example/charts"},
	})

	if _, err := reconcileOnce(cat, repo, []repoclient.Chart{chart("podinfo", "6.7.1")}); err != nil {
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
	cat := adapter.NewApplicationCatalog(c, c)
	repo := applicationRepo("team-a", "stable")

	if _, err := reconcileOnce(cat, repo, []repoclient.Chart{chart("podinfo", "1.0.0"), chart("nginx", "1.0.0")}); err != nil {
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
	cat := adapter.NewApplicationCatalog(c, c)
	repo := applicationRepo("team-a", "stable")

	if _, err := reconcileOnce(cat, repo, []repoclient.Chart{chart("podinfo", "6.7.1")}); err != nil {
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

	_, err := adapter.NewClusterApplicationCatalog(c, c).
		Snapshot(context.Background(), adapter.NewClusterApplicationRepository(
			&helmv1alpha1.HelmClusterApplicationRepository{
				ObjectMeta: metav1.ObjectMeta{Name: "shared", UID: types.UID("shared")},
			},
		))
	if err == nil {
		t.Fatal("the catalog read must report the list failure")
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
	cat := adapter.NewAddonCatalog(c, c)
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
	cat := adapter.NewAddonCatalog(c, c)
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
	cat := adapter.NewAddonCatalog(c, c)
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

	cat = adapter.NewAddonCatalog(c, c)
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
	cat := adapter.NewAddonCatalog(c, c)
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
	cat := adapter.NewAddonCatalog(c, c)

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
	cat := adapter.NewAddonCatalog(c, c)

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

// TestMigrationLeavesADestinationAnotherPassWroteAlone pins the one thing the status
// carry-over must never do. The copy replaces the destination's whole version array,
// and the guard that allows it — the destination holds no versions — is a statement
// about the read this pass took, not about the catalog: another pass can write that
// object between the read and the copy. What it writes is worth the most where it is
// easiest to lose, a re-examination mark being the extreme case, since the force
// annotation that asked for it is consumed by the pass that recorded it.
//
// The copy therefore carries the resource version it was computed from. A destination
// that moved on rejects it, the migration is left unfinished — legacy object included,
// because deleting it after a rejected copy would throw away the only copy of a version
// a consumer still pins — and the next pass reads both objects again.
//
// That retry is where both objects have to survive the other pass's write: the
// destination keeps everything it was given, and the version only the legacy object
// records — pinned by a consumer, and no longer offered by the repository, which is why
// it was retained at all — comes over before the legacy object is deleted. Losing it
// costs the release the media type it resolves through, with nothing left anywhere to
// reconstruct it from.
func TestMigrationLeavesADestinationAnotherPassWroteAlone(t *testing.T) {
	const (
		legacyName = "example-chart-podinfo"
		mediaType  = "application/vnd.cncf.helm.chart.content.v1.tar+gzip"
	)

	currentName := naming.HelmClusterAddonChartName("example", "podinfo")

	// The competing writer runs once, just before the copy lands: it records on the
	// destination a refresh whose annotation has already been consumed.
	overtaken := false
	c := newAddonClientWithInterceptor(t, interceptor.Funcs{
		SubResourcePatch: func(ctx context.Context, cl client.Client, sub string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
			if obj.GetName() == currentName && !overtaken {
				overtaken = true

				written := &helmv1alpha1.HelmClusterAddonChart{}
				if err := cl.Get(ctx, client.ObjectKey{Name: currentName}, written); err != nil {
					return err
				}

				written.Status.Versions = []helmv1alpha1.ChartVersion{
					{Version: "2.0.0", MediaType: mediaType, ReexaminationRequired: true},
				}

				if err := cl.Status().Update(ctx, written); err != nil {
					return err
				}
			}

			return cl.SubResource(sub).Patch(ctx, obj, patch, opts...)
		},
	},
		legacyAddonChart(legacyName, "example", "podinfo",
			helmv1alpha1.ChartVersion{Version: "1.0.0", MediaType: mediaType},
		),
		addonConsumer("consumer", "example", "podinfo", "1.0.0"),
	)
	cat := adapter.NewAddonCatalog(c, c)

	if err := cat.MigrateNames(context.Background(), addonRepo()); !apierrors.IsConflict(err) {
		t.Fatalf("MigrateNames returned %v, want the copy rejected as a conflict: the destination moved on since it was read", err)
	}

	if err := c.Get(context.Background(), client.ObjectKey{Name: legacyName}, &helmv1alpha1.HelmClusterAddonChart{}); err != nil {
		t.Fatalf("legacy object err = %v, want it kept: a rejected copy has migrated nothing", err)
	}

	written := &helmv1alpha1.HelmClusterAddonChart{}
	if err := c.Get(context.Background(), client.ObjectKey{Name: currentName}, written); err != nil {
		t.Fatalf("reading the destination: %v", err)
	}
	if len(written.Status.Versions) != 1 || written.Status.Versions[0].Version != "2.0.0" {
		t.Fatalf("destination versions = %+v, want what the other pass wrote, untouched", written.Status.Versions)
	}
	if !written.Status.Versions[0].ReexaminationRequired {
		t.Fatal("a rejected copy must not clear a mark the other pass recorded: the force asking for it is already consumed")
	}

	// The next pass reads both objects again: the destination now holds versions, so
	// nothing is copied over them, and the migration finishes.
	if err := cat.MigrateNames(context.Background(), addonRepo()); err != nil {
		t.Fatalf("the pass after the conflict returned %v", err)
	}

	if err := c.Get(context.Background(), client.ObjectKey{Name: legacyName}, &helmv1alpha1.HelmClusterAddonChart{}); !apierrors.IsNotFound(err) {
		t.Fatalf("legacy object err = %v, want it deleted once the migration finished", err)
	}
	if err := c.Get(context.Background(), client.ObjectKey{Name: currentName}, written); err != nil {
		t.Fatalf("reading the destination after the retry: %v", err)
	}

	migrated := make(map[string]helmv1alpha1.ChartVersion, len(written.Status.Versions))
	for _, version := range written.Status.Versions {
		migrated[version.Version] = version
	}
	if len(migrated) != 2 {
		t.Fatalf("destination versions = %+v, want both the other pass's entry and the one only the legacy object held", written.Status.Versions)
	}
	if !migrated["2.0.0"].ReexaminationRequired {
		t.Fatal("the retry must keep the mark the other pass recorded: it is still the only record of that request")
	}
	if migrated["1.0.0"].MediaType != mediaType {
		t.Fatalf("1.0.0 = %+v, want the pinned version carried over with its media type before the legacy object was deleted", migrated["1.0.0"])
	}
}

// TestMigrationKeepsTheDestinationOwnedByTheRepository pins the metadata a created
// destination inherits. Catalog objects are garbage collected with the repository that
// owns them, and the migration is the one path that creates one outside the
// synchronization: a chart the repository no longer offers is migrated but never
// reconciled again, so a destination created without the owner reference keeps it
// forever and outlives the repository as an orphan nothing deletes.
func TestMigrationKeepsTheDestinationOwnedByTheRepository(t *testing.T) {
	const legacyName = "example-chart-gone"

	repo := adapter.NewAddonRepository(&helmv1alpha1.HelmClusterAddonRepository{
		ObjectMeta: metav1.ObjectMeta{Name: "example", UID: "example-uid"},
	})

	legacy := legacyAddonChart(legacyName, "example", "gone",
		helmv1alpha1.ChartVersion{Version: "1.0.0", MediaType: "application/vnd.cncf.helm.chart.content.v1.tar+gzip"},
	)
	legacy.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(repo.Object(), repo.OwnerGVK())}

	c := newAddonClient(t, legacy, addonConsumer("consumer", "example", "gone", "1.0.0"))
	cat := adapter.NewAddonCatalog(c, c)

	// The migration alone, without a synchronization behind it: the repository does not
	// offer this chart, so nothing else ever stamps the destination's metadata.
	if err := cat.MigrateNames(context.Background(), repo); err != nil {
		t.Fatalf("MigrateNames returned %v", err)
	}

	got := &helmv1alpha1.HelmClusterAddonChart{}
	if err := c.Get(context.Background(), client.ObjectKey{Name: naming.HelmClusterAddonChartName("example", "gone")}, got); err != nil {
		t.Fatalf("reading the migrated object: %v", err)
	}

	if len(got.OwnerReferences) != 1 {
		t.Fatalf("owner references = %v, want the destination owned by the repository the legacy object belonged to", got.OwnerReferences)
	}
	owner := got.OwnerReferences[0]
	if owner.Kind != helmv1alpha1.HelmClusterAddonRepositoryKind || owner.Name != "example" ||
		owner.UID != "example-uid" || owner.Controller == nil || !*owner.Controller {
		t.Fatalf("owner = %+v, want a controller reference to HelmClusterAddonRepository/example", owner)
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
	cat := adapter.NewAddonCatalog(c, c)
	repo := addonRepo()

	// The unwritable chart comes first, so a pass that stopped on it would also be
	// missing the one after it.
	result, err := reconcileOnce(cat, repo, []repoclient.Chart{
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

			result, err := reconcileOnce(adapter.NewAddonCatalog(c, c), repo, charts)
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
	cat := adapter.NewApplicationCatalog(c, c)
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
	if _, err := reconcileOnce(cat, repo, []repoclient.Chart{resolved}); err != nil {
		t.Fatalf("the resolving pass returned %v", err)
	}

	// The next pass cannot reach the registry for that tag: no media type, no
	// reference, only the transport failure.
	pending := repoclient.Chart{Name: "podinfo", Versions: []repoclient.ChartVersion{{
		Version:            semver.MustParse("6.7.1"),
		UnavailableReason:  helmv1alpha1.UnavailableReasonResolvePending,
		UnavailableMessage: "GET https://registry.example.invalid/v2/: dial tcp: i/o timeout",
	}}}
	if _, err := reconcileOnce(cat, repo, []repoclient.Chart{pending}); err != nil {
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

	known, err := knownCharts(cat, repo)
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
	if _, err := reconcileOnce(cat, repo, []repoclient.Chart{resolved}); err != nil {
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
	cat := adapter.NewApplicationCatalog(c, c)
	repo := applicationRepo("team-a", "stable")

	complete := repoclient.Chart{Name: "podinfo", Versions: []repoclient.ChartVersion{
		{Version: semver.MustParse("6.7.1"), MediaType: mediaType},
		{Version: semver.MustParse("6.7.0"), MediaType: mediaType},
	}}
	if _, err := reconcileOnce(cat, repo, []repoclient.Chart{complete}); err != nil {
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
	if _, err := reconcileOnce(cat, repo, []repoclient.Chart{incomplete}); err != nil {
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
	known, err := knownCharts(cat, repo)
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
	cat := adapter.NewApplicationCatalog(c, c)
	repo := applicationRepo("team-a", "stable")

	if _, err := reconcileOnce(cat, repo, []repoclient.Chart{chart("podinfo", "1.0.0"), chart("nginx", "1.0.0")}); err != nil {
		t.Fatalf("the first pass returned %v", err)
	}

	incomplete := chart("podinfo", "1.0.0")
	incomplete.UnexaminedVersions = []string{"1.0.1"}

	if _, err := reconcileOnce(cat, repo, []repoclient.Chart{incomplete}); err != nil {
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
	if _, err := reconcileOnce(cat, repo, []repoclient.Chart{chart("podinfo", "1.0.0")}); err != nil {
		t.Fatalf("the complete pass returned %v", err)
	}
	if err := c.List(context.Background(), &charts, client.InNamespace("team-a")); err != nil {
		t.Fatalf("listing charts: %v", err)
	}
	if len(charts.Items) != 1 || charts.Items[0].Labels[helmv1alpha1.LabelChartName] != "podinfo" {
		t.Fatalf("charts = %v, want only podinfo after a complete pass", charts.Items)
	}
}

// TestAForcedPassKeepsTheMarkOnTheVersionsItDidNotReach pins where the work a forced
// refresh did not finish waits. A forced read ignores every recorded verdict, which is
// the only way a tag that was re-pushed with different content is ever looked at again;
// an ordinary pass skips it, because its media type is recorded and that is what keeps a
// steady-state synchronization cheap. So a forced read that ran out of its budget leaves
// a question open for every version it did not reach, and the catalog entry of that
// version is the only place the question can wait: the force annotation is consumed by
// the pass it triggered, and the count in the repository status names no versions.
//
// The request is written onto every recorded version before the read
// (MarkForReexamination), so the merge never sets the mark; what it decides is which
// half of the request this pass has done — cleared on what it examined, kept on what it
// deferred.
//
// The mark is not a verdict. The entry keeps its media type and stays usable while it
// carries the mark, so a release already deploying that version resolves it exactly as
// before while the refresh finishes over the next passes.
func TestAForcedPassKeepsTheMarkOnTheVersionsItDidNotReach(t *testing.T) {
	const mediaType = "application/vnd.cncf.helm.chart.content.v1.tar+gzip"

	c := newClient(t)
	cat := adapter.NewApplicationCatalog(c, c)
	repo := applicationRepo("team-a", "stable")
	key := client.ObjectKey{Namespace: "team-a", Name: naming.ApplicationChartName("stable", "podinfo")}

	complete := repoclient.Chart{Name: "podinfo", Versions: []repoclient.ChartVersion{
		{Version: semver.MustParse("6.7.1"), MediaType: mediaType},
		{Version: semver.MustParse("6.7.0"), MediaType: mediaType},
	}}
	if _, err := reconcileOnce(cat, repo, []repoclient.Chart{complete}); err != nil {
		t.Fatalf("the steady-state pass returned %v", err)
	}

	// The refresh is forced: every recorded verdict is now owed a fresh one.
	pass := forcedPass(t, cat, repo)

	// The forced pass: it re-examined 6.7.0 and ran out of its budget before reaching
	// 6.7.1 — the version someone re-pushed and forced the refresh for.
	forced := repoclient.Chart{
		Name:               "podinfo",
		Versions:           []repoclient.ChartVersion{{Version: semver.MustParse("6.7.0"), MediaType: mediaType}},
		UnexaminedVersions: []string{"6.7.1"},
	}
	if _, err := pass.Reconcile(context.Background(), []repoclient.Chart{forced}); err != nil {
		t.Fatalf("the forced pass returned %v", err)
	}

	marked := versionsByName(t, c, key)
	if !marked["6.7.1"].ReexaminationRequired {
		t.Fatal("a forced pass must keep the mark on the version it did not reach: nothing else re-examines a recorded media type")
	}
	if marked["6.7.0"].ReexaminationRequired {
		t.Fatal("a version the forced pass examined must not be marked")
	}
	if got := marked["6.7.1"]; got.MediaType != mediaType || got.UnavailableReason != "" {
		t.Fatalf("marked version = %+v, want the recorded verdict untouched: a release still deploys from it", got)
	}

	known, err := knownCharts(cat, repo)
	if err != nil {
		t.Fatalf("Known returned %v", err)
	}
	opts := repoclient.FetchOptions{Known: known}
	if !opts.NeedsExamination("podinfo", "6.7.1") {
		t.Fatal("the next pass must examine the marked version, forced or not")
	}
	if opts.NeedsExamination("podinfo", "6.7.0") {
		t.Fatal("the next pass must still skip the version the forced pass examined")
	}

	// The next pass is an ordinary one — the request was consumed by the forced pass —
	// and it examines exactly what the mark asked for. Answering clears the mark, so
	// the refresh ends here rather than repeating forever.
	repushed := repoclient.Chart{Name: "podinfo", Versions: []repoclient.ChartVersion{
		{Version: semver.MustParse("6.7.1"), MediaType: "application/tar+gzip"},
		{Version: semver.MustParse("6.7.0"), MediaType: mediaType},
	}}
	if _, err := reconcileOnce(cat, repo, []repoclient.Chart{repushed}); err != nil {
		t.Fatalf("the pass that finished the refresh returned %v", err)
	}

	refreshed := versionsByName(t, c, key)
	if refreshed["6.7.1"].ReexaminationRequired {
		t.Fatal("the pass that examined the marked version must clear the mark")
	}
	if got := refreshed["6.7.1"].MediaType; got != "application/tar+gzip" {
		t.Fatalf("6.7.1 media type = %q, want the one the re-pushed tag actually holds", got)
	}
}

// TestAnIncompletePassMarksNothingItself is the bound on the rule above. The merge only
// ever clears the mark or keeps what is already recorded; it never puts one on. An
// incomplete pass over a catalog nothing marked therefore leaves it unmarked, whatever
// it failed to reach — a pass that reads no verdict it is entitled to skip owes nothing,
// and marking what it ran out of budget for would turn every incomplete synchronization
// of a large repository into a full re-examination of it.
func TestAnIncompletePassMarksNothingItself(t *testing.T) {
	const mediaType = "application/vnd.cncf.helm.chart.content.v1.tar+gzip"

	c := newClient(t)
	cat := adapter.NewApplicationCatalog(c, c)
	repo := applicationRepo("team-a", "stable")
	key := client.ObjectKey{Namespace: "team-a", Name: naming.ApplicationChartName("stable", "podinfo")}

	complete := repoclient.Chart{Name: "podinfo", Versions: []repoclient.ChartVersion{
		{Version: semver.MustParse("6.7.1"), MediaType: mediaType},
	}}
	if _, err := reconcileOnce(cat, repo, []repoclient.Chart{complete}); err != nil {
		t.Fatalf("the first pass returned %v", err)
	}

	incomplete := repoclient.Chart{Name: "podinfo", UnexaminedVersions: []string{"6.7.1"}}
	if _, err := reconcileOnce(cat, repo, []repoclient.Chart{incomplete}); err != nil {
		t.Fatalf("the incomplete pass returned %v", err)
	}

	if versionsByName(t, c, key)["6.7.1"].ReexaminationRequired {
		t.Fatal("the merge must mark nothing: only a forced refresh writes the mark, and it writes it before the read")
	}
}

// TestAMarkedVersionTheRepositoryNoLongerOffersIsReportedRemoved covers the one way a
// marked version can stop being listed: the tag is deleted from the registry before a
// pass gets to it. The mark asks for an examination that can now never happen, so the
// pass that sees the version gone drops it — the entry a consumer keeps alive must not
// carry a request no pass can clear, or the repository would re-examine it forever.
func TestAMarkedVersionTheRepositoryNoLongerOffersIsReportedRemoved(t *testing.T) {
	const mediaType = "application/vnd.cncf.helm.chart.content.v1.tar+gzip"

	c := newClient(t)
	cat := adapter.NewApplicationCatalog(c, c)
	repo := applicationRepo("team-a", "stable")
	key := client.ObjectKey{Namespace: "team-a", Name: naming.ApplicationChartName("stable", "podinfo")}

	app := &helmv1alpha1.HelmApplication{
		ObjectMeta: metav1.ObjectMeta{Name: "podinfo", Namespace: "team-a"},
		Spec: helmv1alpha1.HelmApplicationSpec{
			Chart: helmv1alpha1.HelmApplicationChartRef{Name: "podinfo", Repository: "stable", Version: "6.7.1"},
		},
	}
	if err := c.Create(context.Background(), app); err != nil {
		t.Fatalf("creating the application using the version: %v", err)
	}

	complete := repoclient.Chart{Name: "podinfo", Versions: []repoclient.ChartVersion{
		{Version: semver.MustParse("6.7.1"), MediaType: mediaType},
	}}
	if _, err := reconcileOnce(cat, repo, []repoclient.Chart{complete}); err != nil {
		t.Fatalf("the steady-state pass returned %v", err)
	}

	pass := forcedPass(t, cat, repo)

	forced := repoclient.Chart{Name: "podinfo", UnexaminedVersions: []string{"6.7.1"}}
	if _, err := pass.Reconcile(context.Background(), []repoclient.Chart{forced}); err != nil {
		t.Fatalf("the forced pass returned %v", err)
	}
	if !versionsByName(t, c, key)["6.7.1"].ReexaminationRequired {
		t.Fatal("the fixture must reach a marked version first")
	}

	// The tag is gone from the registry, and this pass saw the whole listing.
	gone := repoclient.Chart{Name: "podinfo", Versions: []repoclient.ChartVersion{
		{Version: semver.MustParse("6.8.0"), MediaType: mediaType},
	}}
	if _, err := reconcileOnce(cat, repo, []repoclient.Chart{gone}); err != nil {
		t.Fatalf("the pass that no longer lists the version returned %v", err)
	}

	removed := versionsByName(t, c, key)["6.7.1"]
	if removed.UnavailableReason != helmv1alpha1.UnavailableReasonRemovedFromRepository {
		t.Fatalf("6.7.1 reason = %q, want %q", removed.UnavailableReason, helmv1alpha1.UnavailableReasonRemovedFromRepository)
	}
	if removed.ReexaminationRequired {
		t.Fatal("a version the repository no longer offers must not stay marked: no pass can ever clear it")
	}
	if removed.MediaType != mediaType {
		t.Fatalf("6.7.1 media type = %q, want the recorded one: the application still deploys it", removed.MediaType)
	}
}

// TestForcingARefreshMarksARetainedRemovedVersionAndALaterPassClearsIt pins that a
// forced refresh exempts nothing it recorded. A version the repository stopped offering
// is retained because a consumer still deploys it, and it keeps the media type of the
// artifact that was deleted — so re-pushing that exact tag is precisely the change a
// forced refresh exists to notice. Exempting it would let a forced pass that ran out of
// its budget leave the entry unmarked and unexamined, and the ordinary pass after it
// would republish the deleted artifact's media type without ever probing the tag.
//
// The mark is not a dead end: this repository still does not offer the version, and the
// pass that says so clears it while reporting the version removed, so nothing claims
// work forever in a status a user reads.
func TestForcingARefreshMarksARetainedRemovedVersionAndALaterPassClearsIt(t *testing.T) {
	const mediaType = "application/vnd.cncf.helm.chart.content.v1.tar+gzip"

	c := newClient(t)
	cat := adapter.NewApplicationCatalog(c, c)
	repo := applicationRepo("team-a", "stable")
	key := client.ObjectKey{Namespace: "team-a", Name: naming.ApplicationChartName("stable", "podinfo")}

	app := &helmv1alpha1.HelmApplication{
		ObjectMeta: metav1.ObjectMeta{Name: "podinfo", Namespace: "team-a"},
		Spec: helmv1alpha1.HelmApplicationSpec{
			Chart: helmv1alpha1.HelmApplicationChartRef{Name: "podinfo", Repository: "stable", Version: "6.7.0"},
		},
	}
	if err := c.Create(context.Background(), app); err != nil {
		t.Fatalf("creating the application using the version: %v", err)
	}

	complete := repoclient.Chart{Name: "podinfo", Versions: []repoclient.ChartVersion{
		{Version: semver.MustParse("6.7.1"), MediaType: mediaType},
		{Version: semver.MustParse("6.7.0"), MediaType: mediaType},
	}}
	if _, err := reconcileOnce(cat, repo, []repoclient.Chart{complete}); err != nil {
		t.Fatalf("the steady-state pass returned %v", err)
	}

	// 6.7.0 is deleted from the registry and survives only because the application
	// deploys it.
	dropped := repoclient.Chart{Name: "podinfo", Versions: []repoclient.ChartVersion{
		{Version: semver.MustParse("6.7.1"), MediaType: mediaType},
	}}
	if _, err := reconcileOnce(cat, repo, []repoclient.Chart{dropped}); err != nil {
		t.Fatalf("the pass that no longer lists the version returned %v", err)
	}
	if versionsByName(t, c, key)["6.7.0"].UnavailableReason != helmv1alpha1.UnavailableReasonRemovedFromRepository {
		t.Fatal("the fixture must reach a retained, removed version first")
	}

	pass := forcedPass(t, cat, repo)

	marked := versionsByName(t, c, key)
	if !marked["6.7.0"].ReexaminationRequired {
		t.Fatal("a retained removed version must be marked too: a re-push of its tag is what the refresh looks for")
	}
	if !marked["6.7.1"].ReexaminationRequired {
		t.Fatal("the version the repository still offers must carry the request")
	}
	if got := marked["6.7.0"]; got.MediaType != mediaType || got.UnavailableReason != helmv1alpha1.UnavailableReasonRemovedFromRepository {
		t.Fatalf("marked version = %+v, want the recorded entry untouched: the consumer still deploys from it", got)
	}

	// The repository still does not offer 6.7.0, and the pass that sees the whole
	// repository says so: the mark ends there rather than sitting in the status forever.
	if _, err := pass.Reconcile(context.Background(), []repoclient.Chart{dropped}); err != nil {
		t.Fatalf("the pass after the mark returned %v", err)
	}

	cleared := versionsByName(t, c, key)
	if cleared["6.7.0"].ReexaminationRequired {
		t.Fatal("a pass that still does not list the version has answered the mark and must clear it")
	}
	if cleared["6.7.0"].UnavailableReason != helmv1alpha1.UnavailableReasonRemovedFromRepository {
		t.Fatalf("version = %+v, want the version still reported removed", cleared["6.7.0"])
	}
}

// TestAChartTheFetchNoLongerOffersHasItsRetainedVersionsRecordedRemoved covers the
// chart that disappears whole. A chart the repository stopped offering is kept as long
// as a consumer pins a version of it, and it is then in no fetched list, so the merge
// the main loop runs never reaches it: its versions keep the media type recorded when
// the chart was last listed, with no unavailability reason next to it. That is the one
// state a re-push survives — every tag deleted and published again with different
// content behind it — because an ordinary pass reads a recorded media type as
// authoritative and carries it forward (FetchOptions.NeedsExamination), so the catalog
// would go on publishing the verdict of an artifact that no longer exists.
//
// The pass that saw the whole repository without the chart answers it instead, with the
// merge a listed chart with an empty version list would get: the pinned version is
// reported removed and keeps everything the consumer's OCIRepository is built from, an
// unreferenced version is dropped as it would be anywhere else, and the re-examination
// mark — a request no pass could answer while the chart is unlisted, since probe
// candidates come from the listing — is cleared by having been answered.
func TestAChartTheFetchNoLongerOffersHasItsRetainedVersionsRecordedRemoved(t *testing.T) {
	const mediaType = "application/vnd.cncf.helm.chart.content.v1.tar+gzip"

	c := newClient(t)
	cat := adapter.NewApplicationCatalog(c, c)
	repo := applicationRepo("team-a", "stable")
	key := client.ObjectKey{Namespace: "team-a", Name: naming.ApplicationChartName("stable", "legacy")}

	app := &helmv1alpha1.HelmApplication{
		ObjectMeta: metav1.ObjectMeta{Name: "legacy", Namespace: "team-a"},
		Spec: helmv1alpha1.HelmApplicationSpec{
			Chart: helmv1alpha1.HelmApplicationChartRef{Name: "legacy", Repository: "stable", Version: "1.0.0"},
		},
	}
	if err := c.Create(context.Background(), app); err != nil {
		t.Fatalf("creating the application pinning the chart: %v", err)
	}

	pinned := repoclient.Chart{Name: "legacy", Versions: []repoclient.ChartVersion{
		{Version: semver.MustParse("1.1.0"), MediaType: mediaType, OCIRef: "registry.example.invalid/legacy:1.1.0"},
		{Version: semver.MustParse("1.0.0"), MediaType: mediaType, OCIRef: "registry.example.invalid/legacy:1.0.0"},
	}}
	offered := repoclient.Chart{Name: "podinfo", Versions: []repoclient.ChartVersion{
		{Version: semver.MustParse("6.7.1"), MediaType: mediaType},
	}}
	if _, err := reconcileOnce(cat, repo, []repoclient.Chart{pinned, offered}); err != nil {
		t.Fatalf("the steady-state pass returned %v", err)
	}

	pass := forcedPass(t, cat, repo)
	if !versionsByName(t, c, key)["1.0.0"].ReexaminationRequired {
		t.Fatal("the fixture must reach a marked version of the pinned chart first")
	}

	// The repository no longer offers the chart at all, and this pass saw the whole
	// repository.
	if _, err := pass.Reconcile(context.Background(), []repoclient.Chart{offered}); err != nil {
		t.Fatalf("the pass that no longer offers the chart returned %v", err)
	}

	kept := versionsByName(t, c, key)
	if len(kept) != 1 {
		t.Fatalf("recorded versions = %+v, want only the one a consumer pins: the rest is prunable like anywhere else", kept)
	}
	if got := kept["1.0.0"].UnavailableReason; got != helmv1alpha1.UnavailableReasonRemovedFromRepository {
		t.Fatalf("unavailability reason = %q, want the version reported removed: nothing else records that the chart is gone", got)
	}
	if got := kept["1.0.0"]; got.MediaType != mediaType || got.OCIRef != "registry.example.invalid/legacy:1.0.0" {
		t.Fatalf("kept version = %+v, want the artifact information intact: the application still deploys it", got)
	}
	if kept["1.0.0"].ReexaminationRequired {
		t.Fatal("a chart no fetch offers any more must not keep a mark: no pass can ever examine its versions")
	}

	// What the recorded reason buys: the tag is a probe candidate again the moment the
	// chart is listed once more, without a forced refresh, because the listing offering
	// it is the evidence that there is new content behind the verdict.
	known, err := knownCharts(cat, repo)
	if err != nil {
		t.Fatalf("reading the recorded verdicts returned %v", err)
	}
	if !(repoclient.FetchOptions{Known: known}).NeedsExamination("legacy", "1.0.0") {
		t.Fatal("a re-pushed tag of a chart that came back must be examined by an ordinary pass")
	}
}

// TestAFailedAbsenceWriteIsReportedAndRetried pins the failure half of the answer a
// chart the repository stopped offering gets. Writing that answer is the only thing that
// stops the retained versions from going on publishing the media type of an artifact the
// repository no longer holds, and it is a status patch like any other: it can be rejected
// by a conflict, a webhook or an unavailable API server. Swallowing that would leave the
// pass reporting a successful synchronization over a catalog that still says the chart is
// fine, with the next pass no more likely to notice — nothing else reconciles an unlisted
// chart. So the error has to reach the caller, the status it could not write has to be
// left exactly as it was rather than half-applied, and the pass that runs after the
// failure has to finish the work.
func TestAFailedAbsenceWriteIsReportedAndRetried(t *testing.T) {
	const mediaType = "application/vnd.cncf.helm.chart.content.v1.tar+gzip"

	absentName := naming.ApplicationChartName("stable", "legacy")
	failAbsenceWrite := false

	// The fault is aimed at the retained chart's status alone: a fault that failed every
	// status patch would be answered by the fetched charts' own writes, several steps
	// before the absence is ever recorded.
	c := newClientWithInterceptor(t, interceptor.Funcs{
		SubResourcePatch: func(
			ctx context.Context,
			cl client.Client,
			sub string,
			obj client.Object,
			patch client.Patch,
			opts ...client.SubResourcePatchOption,
		) error {
			if failAbsenceWrite && obj.GetName() == absentName {
				return errors.New("the api server rejected the status patch")
			}

			return cl.SubResource(sub).Patch(ctx, obj, patch, opts...)
		},
	})
	cat := adapter.NewApplicationCatalog(c, c)
	repo := applicationRepo("team-a", "stable")
	key := client.ObjectKey{Namespace: "team-a", Name: absentName}

	app := &helmv1alpha1.HelmApplication{
		ObjectMeta: metav1.ObjectMeta{Name: "legacy", Namespace: "team-a"},
		Spec: helmv1alpha1.HelmApplicationSpec{
			Chart: helmv1alpha1.HelmApplicationChartRef{Name: "legacy", Repository: "stable", Version: "1.0.0"},
		},
	}
	if err := c.Create(context.Background(), app); err != nil {
		t.Fatalf("creating the application pinning the chart: %v", err)
	}

	pinned := repoclient.Chart{Name: "legacy", Versions: []repoclient.ChartVersion{
		{Version: semver.MustParse("1.0.0"), MediaType: mediaType, OCIRef: "registry.example.invalid/legacy:1.0.0"},
	}}
	offered := repoclient.Chart{Name: "podinfo", Versions: []repoclient.ChartVersion{
		{Version: semver.MustParse("6.7.1"), MediaType: mediaType},
	}}
	if _, err := reconcileOnce(cat, repo, []repoclient.Chart{pinned, offered}); err != nil {
		t.Fatalf("the steady-state pass returned %v", err)
	}

	before := versionsByName(t, c, key)
	if got := before["1.0.0"]; got.UnavailableReason != "" || got.MediaType != mediaType {
		t.Fatalf("recorded version = %+v, want the fixture to start from a healthy verdict", got)
	}

	failAbsenceWrite = true

	if _, err := reconcileOnce(cat, repo, []repoclient.Chart{offered}); err == nil {
		t.Fatal("a failed absence write must be reported: the pass would otherwise claim a synchronization that never recorded the chart is gone")
	}

	unchanged := versionsByName(t, c, key)
	if got := unchanged["1.0.0"]; got != before["1.0.0"] {
		t.Fatalf("recorded version = %+v, want it untouched by the failed write (%+v)", got, before["1.0.0"])
	}

	failAbsenceWrite = false

	if _, err := reconcileOnce(cat, repo, []repoclient.Chart{offered}); err != nil {
		t.Fatalf("the pass retrying the absence write returned %v", err)
	}

	recorded := versionsByName(t, c, key)
	if got := recorded["1.0.0"].UnavailableReason; got != helmv1alpha1.UnavailableReasonRemovedFromRepository {
		t.Fatalf("unavailability reason = %q, want the retry to record the absence the failed pass owed", got)
	}
	if got := recorded["1.0.0"]; got.MediaType != mediaType || got.OCIRef != "registry.example.invalid/legacy:1.0.0" {
		t.Fatalf("kept version = %+v, want the artifact information intact: the application still deploys it", got)
	}
}

// TestForcingARefreshLeavesAVersionAwaitingAVerdictUnmarked pins the one entry the
// request is not written onto, and it is left out because it already asks for
// everything the mark would ask for: a version whose probe failed is re-examined by
// every pass (FetchOptions.NeedsExamination), so marking it buys a status write and
// nothing else. It also costs something: the probe order asks the stalling tags last,
// after the never-seen ones and the ones a refresh owes, and a mark would move this tag
// out of that last group and let a block of permanently stalling tags spend the budget
// ahead of work that can finish.
func TestForcingARefreshLeavesAVersionAwaitingAVerdictUnmarked(t *testing.T) {
	const mediaType = "application/vnd.cncf.helm.chart.content.v1.tar+gzip"

	c := newClient(t)
	cat := adapter.NewApplicationCatalog(c, c)
	repo := applicationRepo("team-a", "stable")
	key := client.ObjectKey{Namespace: "team-a", Name: naming.ApplicationChartName("stable", "podinfo")}

	pending := repoclient.Chart{Name: "podinfo", Versions: []repoclient.ChartVersion{
		{Version: semver.MustParse("6.7.1"), MediaType: mediaType},
		{
			Version:            semver.MustParse("6.7.0"),
			UnavailableReason:  helmv1alpha1.UnavailableReasonResolvePending,
			UnavailableMessage: "GET https://registry.example.invalid/v2/: dial tcp: i/o timeout",
		},
	}}
	if _, err := reconcileOnce(cat, repo, []repoclient.Chart{pending}); err != nil {
		t.Fatalf("the pass whose probe failed returned %v", err)
	}

	pass := forcedPass(t, cat, repo)

	marked := versionsByName(t, c, key)
	if marked["6.7.0"].ReexaminationRequired {
		t.Fatal("a version still awaiting a verdict is re-examined anyway: the mark only costs a write")
	}
	if !marked["6.7.1"].ReexaminationRequired {
		t.Fatal("the version with a verdict to distrust must carry the request")
	}

	// What the rest of the pass decides from says the same thing as the status: the
	// snapshot the mark was written on is the snapshot the fetch and the write use.
	known := pass.Known()
	if known["podinfo"]["6.7.0"].ReexaminationRequired {
		t.Fatal("a version left unmarked must not be offered to the fetch as owing a fresh verdict")
	}
	if !known["podinfo"]["6.7.1"].ReexaminationRequired {
		t.Fatal("the marked version must reach the fetch as marked: nothing re-reads the catalog to find out")
	}
}

// TestForcingARefreshAgainRewritesNothing covers the cost of a request that cannot be
// carried out. A forced pass that never reaches the registry is retried on the backoff,
// and each retry marks again; rewriting the status of every chart object of the
// repository on each of those retries would multiply a broken registry into a write
// storm against the API server, for a status that already says what the mark says.
//
// Writing nothing is only safe because the snapshot the decision is taken from is read
// through the API server: "already marked" is then a statement about the catalog, not
// about one reader's copy of it. TestASecondForceIsRecordedWhenTheCachedCatalogStillShowsTheFirst
// is the other half of that rule.
func TestForcingARefreshAgainRewritesNothing(t *testing.T) {
	patches := 0
	c := newAddonClientWithInterceptor(t, interceptor.Funcs{
		SubResourcePatch: func(ctx context.Context, cl client.Client, sub string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
			patches++

			return cl.SubResource(sub).Patch(ctx, obj, patch, opts...)
		},
	})
	cat := adapter.NewAddonCatalog(c, c)
	repo := addonRepo()

	if err := syncCatalog(cat, repo, chart("podinfo", "1.0.0")); err != nil {
		t.Fatalf("the steady-state pass returned %v", err)
	}

	forcedPass(t, cat, repo)

	afterFirst := patches

	second := forcedPass(t, cat, repo)

	if !second.Known()["podinfo"]["1.0.0"].ReexaminationRequired {
		t.Fatal("a version already carrying the mark is still owed a fresh verdict: the fetch of this pass has to re-examine it")
	}

	if patches != afterFirst {
		t.Fatalf("status patches = %d, want %d: a mark that is already recorded must not be rewritten", patches, afterFirst)
	}
}

// versionsByName reads the recorded versions of one catalog object, keyed by version.
func versionsByName(t *testing.T, c client.Client, key client.ObjectKey) map[string]helmv1alpha1.ChartVersion {
	t.Helper()

	stored := &helmv1alpha1.HelmApplicationChart{}
	if err := c.Get(context.Background(), key, stored); err != nil {
		t.Fatalf("reading the catalog object: %v", err)
	}

	versions := make(map[string]helmv1alpha1.ChartVersion, len(stored.Status.Versions))
	for _, version := range stored.Status.Versions {
		versions[version.Version] = version
	}

	return versions
}
