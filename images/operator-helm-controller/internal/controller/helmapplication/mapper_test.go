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

package helmapplication

import (
	"context"
	"reflect"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
	"github.com/deckhouse/operator-helm/internal/adapter"
	"github.com/deckhouse/operator-helm/internal/index"
	"github.com/deckhouse/operator-helm/internal/services"
)

func newMapperClient(t *testing.T, objects ...client.Object) client.Client {
	t.Helper()

	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		clientgoscheme.AddToScheme,
		helmv1alpha1.AddToScheme,
	} {
		if err := add(scheme); err != nil {
			t.Fatalf("registering scheme: %v", err)
		}
	}

	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
}

func application(namespace, name string) *helmv1alpha1.HelmApplication {
	return &helmv1alpha1.HelmApplication{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
	}
}

// TestMapRoleToApplications pins that the namespace Role reaches every application
// of its namespace and nothing outside it, and that an unrelated Role carrying our
// label — one left behind by another module, say — enqueues nothing.
func TestMapRoleToApplications(t *testing.T) {
	c := newMapperClient(t,
		application("team-a", "first"),
		application("team-a", "second"),
		application("team-b", "elsewhere"),
	)
	mapper := mapRoleToApplications(c)

	role := &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: services.ApplicationRoleName, Namespace: "team-a"}}
	got := mapper(context.Background(), role)

	want := []reconcile.Request{
		{NamespacedName: types.NamespacedName{Namespace: "team-a", Name: "first"}},
		{NamespacedName: types.NamespacedName{Namespace: "team-a", Name: "second"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("requests = %v, want %v", got, want)
	}

	other := &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: "someone-elses-role", Namespace: "team-a"}}
	if got := mapper(context.Background(), other); got != nil {
		t.Fatalf("a role under another name must map to nothing, got %v", got)
	}
}

// TestMapRoleBindingToApplications pins that a binding reaches the one application
// whose derived service account name it carries — matched on the name rather than on
// the labels, which is what lets a binding stripped of them still be repaired.
func TestMapRoleBindingToApplications(t *testing.T) {
	app := application("team-a", "first")
	c := newMapperClient(t, app, application("team-a", "second"))
	mapper := mapRoleBindingToApplications(c)

	name := adapter.NewApplicationRelease(app).InternalNames().ServiceAccount
	binding := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "team-a"}}

	got := mapper(context.Background(), binding)
	want := []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: "team-a", Name: "first"}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("requests = %v, want %v", got, want)
	}

	foreign := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "someone-elses-binding", Namespace: "team-a"}}
	if got := mapper(context.Background(), foreign); got != nil {
		t.Fatalf("a binding no application is named after must map to nothing, got %v", got)
	}
}

func applicationMapperClient(t *testing.T, objects ...client.Object) client.Client {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := helmv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("registering helm scheme: %v", err)
	}

	return fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objects...).
		WithIndex(&helmv1alpha1.HelmApplication{}, index.ApplicationRepository, index.ApplicationRepositoryIndexer).
		WithIndex(&helmv1alpha1.HelmApplication{}, index.ApplicationChart, index.ApplicationChartIndexer).
		Build()
}

func applicationUsing(namespace, name, repository, clusterRepository string) *helmv1alpha1.HelmApplication {
	return &helmv1alpha1.HelmApplication{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: helmv1alpha1.HelmApplicationSpec{
			Chart: helmv1alpha1.HelmApplicationChartRef{
				Name: "podinfo", Repository: repository, ClusterRepository: clusterRepository, Version: "6.7.1",
			},
		},
	}
}

func requestSet(reqs []reconcile.Request) map[types.NamespacedName]bool {
	out := make(map[types.NamespacedName]bool, len(reqs))
	for _, r := range reqs {
		out[r.NamespacedName] = true
	}

	return out
}

// TestMapRepositoryToApplications pins that a namespaced repository enqueues only the
// applications of its own namespace referencing it, and a cluster repository the
// applications of every namespace referencing it by its field.

func TestMapRepositoryToApplications(t *testing.T) {
	c := applicationMapperClient(t,
		applicationUsing("team-a", "a1", "stable", ""),
		applicationUsing("team-b", "b1", "stable", ""),
		applicationUsing("team-b", "b2", "", "stable"),
	)

	namespaced := mapRepositoryToApplications(c, helmv1alpha1.HelmApplicationRepositoryKind)
	got := namespaced(context.Background(), &helmv1alpha1.HelmApplicationRepository{
		ObjectMeta: metav1.ObjectMeta{Name: "stable", Namespace: "team-a"},
	})
	want := map[types.NamespacedName]bool{{Namespace: "team-a", Name: "a1"}: true}
	if !reflect.DeepEqual(requestSet(got), want) {
		t.Fatalf("namespaced mapping = %v, want %v", got, want)
	}

	cluster := mapRepositoryToApplications(c, helmv1alpha1.HelmClusterApplicationRepositoryKind)
	got = cluster(context.Background(), &helmv1alpha1.HelmClusterApplicationRepository{
		ObjectMeta: metav1.ObjectMeta{Name: "stable"},
	})
	want = map[types.NamespacedName]bool{{Namespace: "team-b", Name: "b2"}: true}
	if !reflect.DeepEqual(requestSet(got), want) {
		t.Fatalf("cluster mapping = %v, want %v", got, want)
	}
}

func TestMapChartToApplications(t *testing.T) {
	c := applicationMapperClient(t,
		applicationUsing("team-a", "a1", "stable", ""),
		applicationUsing("team-a", "other", "stable", ""),
		applicationUsing("team-b", "b1", "stable", ""),
	)
	mapper := mapChartToApplications(c, helmv1alpha1.HelmApplicationRepositoryKind)

	got := mapper(context.Background(), &helmv1alpha1.HelmApplicationChart{
		ObjectMeta: metav1.ObjectMeta{
			Name: "stable-chart-podinfo", Namespace: "team-a",
			Labels: map[string]string{helmv1alpha1.LabelRepositoryName: "stable", helmv1alpha1.LabelChartName: "podinfo"},
		},
	})
	want := map[types.NamespacedName]bool{{Namespace: "team-a", Name: "a1"}: true, {Namespace: "team-a", Name: "other"}: true}
	if !reflect.DeepEqual(requestSet(got), want) {
		t.Fatalf("chart mapping = %v, want %v", got, want)
	}

	if got := mapper(context.Background(), &helmv1alpha1.HelmApplicationChart{
		ObjectMeta: metav1.ObjectMeta{Name: "unlabelled", Namespace: "team-a"},
	}); got != nil {
		t.Fatalf("a chart object without labels cannot be mapped, got %v", got)
	}
}
