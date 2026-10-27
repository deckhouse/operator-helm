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

package index

import (
	"reflect"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
)

func applicationFixture(namespace string, chart helmv1alpha1.HelmApplicationChartRef) *helmv1alpha1.HelmApplication {
	return &helmv1alpha1.HelmApplication{
		ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: namespace},
		Spec:       helmv1alpha1.HelmApplicationSpec{Chart: chart},
	}
}

func addonFixture(repository, chart string) *helmv1alpha1.HelmClusterAddon {
	return &helmv1alpha1.HelmClusterAddon{
		ObjectMeta: metav1.ObjectMeta{Name: "addon"},
		Spec: helmv1alpha1.HelmClusterAddonSpec{
			Chart: helmv1alpha1.HelmClusterAddonChartRef{
				HelmClusterAddonRepository: repository,
				HelmClusterAddonChartName:  chart,
			},
		},
	}
}

func TestApplicationRepositoryIndexerKeysByKindAndNamespace(t *testing.T) {
	tests := []struct {
		name string
		app  *helmv1alpha1.HelmApplication
		want []string
	}{
		{
			name: "a namespaced reference is keyed by the application's own namespace",
			app: applicationFixture("team-a", helmv1alpha1.HelmApplicationChartRef{
				Repository: "stable", Name: "redis",
			}),
			want: []string{ApplicationRepositoryValue(helmv1alpha1.HelmApplicationRepositoryKind, "team-a", "stable")},
		},
		{
			name: "a cluster reference is keyed without a namespace",
			app: applicationFixture("team-a", helmv1alpha1.HelmApplicationChartRef{
				ClusterRepository: "stable", Name: "redis",
			}),
			want: []string{ApplicationRepositoryValue(helmv1alpha1.HelmClusterApplicationRepositoryKind, "", "stable")},
		},
		{
			name: "an object with neither reference is not indexed",
			app:  applicationFixture("team-a", helmv1alpha1.HelmApplicationChartRef{Name: "redis"}),
			want: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ApplicationRepositoryIndexer(tt.app)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("index value = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestApplicationRepositoryIndexerSeparatesSameNamedRepositories(t *testing.T) {
	values := map[string]string{
		"namespaced in team-a": ApplicationRepositoryIndexer(applicationFixture("team-a",
			helmv1alpha1.HelmApplicationChartRef{Repository: "stable", Name: "redis"}))[0],
		"namespaced in team-b": ApplicationRepositoryIndexer(applicationFixture("team-b",
			helmv1alpha1.HelmApplicationChartRef{Repository: "stable", Name: "redis"}))[0],
		"cluster-wide": ApplicationRepositoryIndexer(applicationFixture("team-a",
			helmv1alpha1.HelmApplicationChartRef{ClusterRepository: "stable", Name: "redis"}))[0],
	}

	seen := make(map[string]string, len(values))
	for name, value := range values {
		if other, ok := seen[value]; ok {
			t.Fatalf("%s and %s share the index value %q", other, name, value)
		}
		seen[value] = name
	}
}

func TestApplicationChartIndexerNarrowsTheRepositoryKeyToOneChart(t *testing.T) {
	tests := []struct {
		name string
		app  *helmv1alpha1.HelmApplication
		want []string
	}{
		{
			name: "a namespaced reference",
			app: applicationFixture("team-a", helmv1alpha1.HelmApplicationChartRef{
				Repository: "stable", Name: "redis",
			}),
			want: []string{ApplicationChartValue(helmv1alpha1.HelmApplicationRepositoryKind, "team-a", "stable", "redis")},
		},
		{
			name: "a cluster reference",
			app: applicationFixture("team-a", helmv1alpha1.HelmApplicationChartRef{
				ClusterRepository: "stable", Name: "redis",
			}),
			want: []string{ApplicationChartValue(helmv1alpha1.HelmClusterApplicationRepositoryKind, "", "stable", "redis")},
		},
		{
			name: "an object with neither reference is not indexed",
			app:  applicationFixture("team-a", helmv1alpha1.HelmApplicationChartRef{Name: "redis"}),
			want: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ApplicationChartIndexer(tt.app)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("index value = %v, want %v", got, tt.want)
			}
		})
	}

	redisKey := ApplicationChartIndexer(applicationFixture("team-a",
		helmv1alpha1.HelmApplicationChartRef{Repository: "stable", Name: "redis"}))[0]
	postgresKey := ApplicationChartIndexer(applicationFixture("team-a",
		helmv1alpha1.HelmApplicationChartRef{Repository: "stable", Name: "postgres"}))[0]
	if redisKey == postgresKey {
		t.Fatalf("two charts of one repository share the chart key %q", redisKey)
	}

	teamAKey := ApplicationChartIndexer(applicationFixture("team-a",
		helmv1alpha1.HelmApplicationChartRef{ClusterRepository: "stable", Name: "redis"}))[0]
	teamBKey := ApplicationChartIndexer(applicationFixture("team-b",
		helmv1alpha1.HelmApplicationChartRef{ClusterRepository: "stable", Name: "redis"}))[0]
	if teamAKey != teamBKey {
		t.Fatalf("one cluster chart reference keys differently per namespace: %q and %q", teamAKey, teamBKey)
	}
}

func TestAddonChartIndexerKeysThePairTheWebhookLooksUp(t *testing.T) {
	pair := AddonChartIndexer(addonFixture("stable", "redis"))

	tests := []struct {
		name      string
		addon     *helmv1alpha1.HelmClusterAddon
		wantEqual bool
	}{
		{name: "the same pair", addon: addonFixture("stable", "redis"), wantEqual: true},
		{name: "another chart in the same repository", addon: addonFixture("stable", "postgres")},
		{name: "the same chart in another repository", addon: addonFixture("testing", "redis")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := AddonChartIndexer(tt.addon)
			if equal := reflect.DeepEqual(got, pair); equal != tt.wantEqual {
				t.Fatalf("index value %v against %v: equal = %v, want %v", got, pair, equal, tt.wantEqual)
			}
		})
	}
}

func TestAddonRepositoryIndexerSkipsAnAddonWithoutARepository(t *testing.T) {
	tests := []struct {
		name  string
		addon *helmv1alpha1.HelmClusterAddon
		want  []string
	}{
		{name: "with a repository", addon: addonFixture("stable", "redis"), want: []string{"stable"}},
		{name: "without a repository", addon: addonFixture("", "redis"), want: nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := AddonRepositoryIndexer(tt.addon)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("index value = %v, want %v", got, tt.want)
			}
		})
	}
}
