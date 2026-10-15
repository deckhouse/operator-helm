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

func syncCatalog(cat contract.Catalog, repo contract.Repository, charts ...repoclient.Chart) error {
	if err := cat.MigrateNames(context.Background(), repo); err != nil {
		return err
	}

	_, err := reconcileOnce(cat, repo, charts)

	return err
}

func reconcileOnce(cat contract.Catalog, repo contract.Repository, charts []repoclient.Chart) (contract.CatalogResult, error) {
	snapshot, err := cat.Snapshot(context.Background(), repo)
	if err != nil {
		return contract.CatalogResult{}, err
	}

	return snapshot.Reconcile(context.Background(), charts)
}

func knownCharts(cat contract.Catalog, repo contract.Repository) (repoclient.KnownCharts, error) {
	snapshot, err := cat.Snapshot(context.Background(), repo)
	if err != nil {
		return nil, err
	}

	return snapshot.Known(), nil
}

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
	key := client.ObjectKey{Namespace: "team-a", Name: naming.HelmApplicationChartName("stable", "podinfo")}
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

func TestReconcileRepairsAnObjectStrippedOfItsLabels(t *testing.T) {
	c := newClient(t)
	cat := adapter.NewApplicationCatalog(c, c)
	repo := applicationRepo("team-a", "stable")
	key := client.ObjectKey{Namespace: "team-a", Name: naming.HelmApplicationChartName("stable", "podinfo")}

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

func TestReconcileRejectsPublishingOntoAnObjectCreatedAfterTheRead(t *testing.T) {
	const (
		probed   = "application/tar+gzip"
		repushed = "application/vnd.cncf.helm.chart.content.v1.tar+gzip"
	)

	key := client.ObjectKey{Namespace: "team-a", Name: naming.HelmApplicationChartName("stable", "podinfo")}

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
	key := client.ObjectKey{Name: naming.HelmClusterApplicationChartName("shared", "podinfo")}
	if err := c.Get(context.Background(), key, got); err != nil {
		t.Fatalf("cluster chart object was not created: %v", err)
	}
	if got.OwnerReferences[0].Kind != helmv1alpha1.HelmClusterApplicationRepositoryKind {
		t.Fatalf("owner kind = %q", got.OwnerReferences[0].Kind)
	}
}

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
	if obj.GetNamespace() != "team-a" || obj.GetName() != naming.HelmApplicationChartName("stable", "podinfo") {
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

func TestMigrationLeavesADestinationAnotherPassWroteAlone(t *testing.T) {
	const (
		legacyName = "example-chart-podinfo"
		mediaType  = "application/vnd.cncf.helm.chart.content.v1.tar+gzip"
	)

	currentName := naming.HelmClusterAddonChartName("example", "podinfo")

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

func TestReconcileSkipsAChartWhoseNameCannotBeALabel(t *testing.T) {
	stale := legacyAddonChart(naming.HelmClusterAddonChartName("example", "gone"), "example", "gone",
		helmv1alpha1.ChartVersion{Version: "1.0.0"})

	c := newAddonClientWithInterceptor(t, rejectInvalidLabels(), stale)
	cat := adapter.NewAddonCatalog(c, c)
	repo := addonRepo()

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

func TestReconcileReportsEveryChartItCannotPublish(t *testing.T) {
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
			if len(published.Items) != len(tt.charts)-len(tt.wantSkip) {
				t.Fatalf("catalog holds %d objects, want %d", len(published.Items), len(tt.charts)-len(tt.wantSkip))
			}
		})
	}
}

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

	pending := repoclient.Chart{Name: "podinfo", Versions: []repoclient.ChartVersion{{
		Version:            semver.MustParse("6.7.1"),
		UnavailableReason:  helmv1alpha1.UnavailableReasonResolvePending,
		UnavailableMessage: "GET https://registry.example.invalid/v2/: dial tcp: i/o timeout",
	}}}
	if _, err := reconcileOnce(cat, repo, []repoclient.Chart{pending}); err != nil {
		t.Fatalf("the failing pass returned %v", err)
	}

	stored := &helmv1alpha1.HelmApplicationChart{}
	key := client.ObjectKey{Namespace: "team-a", Name: naming.HelmApplicationChartName("stable", "podinfo")}
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
	key := client.ObjectKey{Namespace: "team-a", Name: naming.HelmApplicationChartName("stable", "podinfo")}
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

	known, err := knownCharts(cat, repo)
	if err != nil {
		t.Fatalf("Known returned %v", err)
	}
	if (repoclient.FetchOptions{Known: known}).NeedsExamination("podinfo", "6.7.1") {
		t.Fatal("a version kept with its verdict must not be examined again")
	}
}

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

func TestAForcedPassKeepsTheMarkOnTheVersionsItDidNotReach(t *testing.T) {
	const mediaType = "application/vnd.cncf.helm.chart.content.v1.tar+gzip"

	c := newClient(t)
	cat := adapter.NewApplicationCatalog(c, c)
	repo := applicationRepo("team-a", "stable")
	key := client.ObjectKey{Namespace: "team-a", Name: naming.HelmApplicationChartName("stable", "podinfo")}

	complete := repoclient.Chart{Name: "podinfo", Versions: []repoclient.ChartVersion{
		{Version: semver.MustParse("6.7.1"), MediaType: mediaType},
		{Version: semver.MustParse("6.7.0"), MediaType: mediaType},
	}}
	if _, err := reconcileOnce(cat, repo, []repoclient.Chart{complete}); err != nil {
		t.Fatalf("the steady-state pass returned %v", err)
	}

	pass := forcedPass(t, cat, repo)

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

func TestAnIncompletePassMarksNothingItself(t *testing.T) {
	const mediaType = "application/vnd.cncf.helm.chart.content.v1.tar+gzip"

	c := newClient(t)
	cat := adapter.NewApplicationCatalog(c, c)
	repo := applicationRepo("team-a", "stable")
	key := client.ObjectKey{Namespace: "team-a", Name: naming.HelmApplicationChartName("stable", "podinfo")}

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

func TestAMarkedVersionTheRepositoryNoLongerOffersIsReportedRemoved(t *testing.T) {
	const mediaType = "application/vnd.cncf.helm.chart.content.v1.tar+gzip"

	c := newClient(t)
	cat := adapter.NewApplicationCatalog(c, c)
	repo := applicationRepo("team-a", "stable")
	key := client.ObjectKey{Namespace: "team-a", Name: naming.HelmApplicationChartName("stable", "podinfo")}

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

func TestForcingARefreshMarksARetainedRemovedVersionAndALaterPassClearsIt(t *testing.T) {
	const mediaType = "application/vnd.cncf.helm.chart.content.v1.tar+gzip"

	c := newClient(t)
	cat := adapter.NewApplicationCatalog(c, c)
	repo := applicationRepo("team-a", "stable")
	key := client.ObjectKey{Namespace: "team-a", Name: naming.HelmApplicationChartName("stable", "podinfo")}

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

func TestAChartTheFetchNoLongerOffersHasItsRetainedVersionsRecordedRemoved(t *testing.T) {
	const mediaType = "application/vnd.cncf.helm.chart.content.v1.tar+gzip"

	c := newClient(t)
	cat := adapter.NewApplicationCatalog(c, c)
	repo := applicationRepo("team-a", "stable")
	key := client.ObjectKey{Namespace: "team-a", Name: naming.HelmApplicationChartName("stable", "legacy")}

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

	known, err := knownCharts(cat, repo)
	if err != nil {
		t.Fatalf("reading the recorded verdicts returned %v", err)
	}
	if !(repoclient.FetchOptions{Known: known}).NeedsExamination("legacy", "1.0.0") {
		t.Fatal("a re-pushed tag of a chart that came back must be examined by an ordinary pass")
	}
}

func TestAFailedAbsenceWriteIsReportedAndRetried(t *testing.T) {
	const mediaType = "application/vnd.cncf.helm.chart.content.v1.tar+gzip"

	absentName := naming.HelmApplicationChartName("stable", "legacy")
	failAbsenceWrite := false

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

func TestForcingARefreshLeavesAVersionAwaitingAVerdictUnmarked(t *testing.T) {
	const mediaType = "application/vnd.cncf.helm.chart.content.v1.tar+gzip"

	c := newClient(t)
	cat := adapter.NewApplicationCatalog(c, c)
	repo := applicationRepo("team-a", "stable")
	key := client.ObjectKey{Namespace: "team-a", Name: naming.HelmApplicationChartName("stable", "podinfo")}

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

	known := pass.Known()
	if known["podinfo"]["6.7.0"].ReexaminationRequired {
		t.Fatal("a version left unmarked must not be offered to the fetch as owing a fresh verdict")
	}
	if !known["podinfo"]["6.7.1"].ReexaminationRequired {
		t.Fatal("the marked version must reach the fetch as marked: nothing re-reads the catalog to find out")
	}
}

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

func TestMigrationCarriesAMarkAndAnIconTheDestinationLacks(t *testing.T) {
	const (
		legacyName = "example-chart-podinfo"
		mediaType  = "application/vnd.cncf.helm.chart.content.v1.tar+gzip"
		iconURL    = "https://example.test/podinfo.png"
	)

	currentName := naming.HelmClusterAddonChartName("example", "podinfo")

	legacy := legacyAddonChart(legacyName, "example", "podinfo",
		helmv1alpha1.ChartVersion{Version: "1.0.0", MediaType: mediaType, ReexaminationRequired: true},
	)
	legacy.Status.IconURL = iconURL

	destination := &helmv1alpha1.HelmClusterAddonChart{
		ObjectMeta: metav1.ObjectMeta{
			Name:   currentName,
			Labels: map[string]string{"repository": "example", "chart": "podinfo"},
		},
		Status: helmv1alpha1.ChartCatalogStatus{
			Versions: []helmv1alpha1.ChartVersion{
				{Version: "1.0.0", MediaType: mediaType},
			},
		},
	}

	c := newAddonClient(t, legacy, destination,
		addonConsumer("consumer", "example", "podinfo", "1.0.0"))
	cat := adapter.NewAddonCatalog(c, c)

	if err := cat.MigrateNames(context.Background(), addonRepo()); err != nil {
		t.Fatalf("MigrateNames returned %v", err)
	}

	written := &helmv1alpha1.HelmClusterAddonChart{}
	if err := c.Get(context.Background(), client.ObjectKey{Name: currentName}, written); err != nil {
		t.Fatalf("reading the destination: %v", err)
	}

	if len(written.Status.Versions) != 1 {
		t.Fatalf("destination versions = %+v, want the shared version alone", written.Status.Versions)
	}
	if !written.Status.Versions[0].ReexaminationRequired {
		t.Fatal("a refresh recorded on the legacy object must survive the rename: nothing has answered it yet")
	}
	if written.Status.IconURL != iconURL {
		t.Fatalf("destination icon = %q, want %q: the icon describes the chart, not one version", written.Status.IconURL, iconURL)
	}
}

func TestMigrationCarriesAMarkWhenNothingElseIsMissing(t *testing.T) {
	const (
		legacyName = "example-chart-podinfo"
		mediaType  = "application/vnd.cncf.helm.chart.content.v1.tar+gzip"
		iconURL    = "https://example.test/podinfo.png"
	)

	currentName := naming.HelmClusterAddonChartName("example", "podinfo")

	legacy := legacyAddonChart(legacyName, "example", "podinfo",
		helmv1alpha1.ChartVersion{Version: "1.0.0", MediaType: mediaType, ReexaminationRequired: true},
	)
	legacy.Status.IconURL = iconURL

	destination := &helmv1alpha1.HelmClusterAddonChart{
		ObjectMeta: metav1.ObjectMeta{
			Name:   currentName,
			Labels: map[string]string{"repository": "example", "chart": "podinfo"},
		},
		Status: helmv1alpha1.ChartCatalogStatus{
			IconURL:  iconURL,
			Versions: []helmv1alpha1.ChartVersion{{Version: "1.0.0", MediaType: mediaType}},
		},
	}

	c := newAddonClient(t, legacy, destination,
		addonConsumer("consumer", "example", "podinfo", "1.0.0"))
	cat := adapter.NewAddonCatalog(c, c)

	if err := cat.MigrateNames(context.Background(), addonRepo()); err != nil {
		t.Fatalf("MigrateNames returned %v", err)
	}

	written := &helmv1alpha1.HelmClusterAddonChart{}
	if err := c.Get(context.Background(), client.ObjectKey{Name: currentName}, written); err != nil {
		t.Fatalf("reading the destination: %v", err)
	}

	if len(written.Status.Versions) != 1 || !written.Status.Versions[0].ReexaminationRequired {
		t.Fatalf("destination versions = %+v, want the mark carried over on its own", written.Status.Versions)
	}
}
