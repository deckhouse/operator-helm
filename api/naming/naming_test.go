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

package naming

import "testing"

func TestHelmClusterAddonChartName(t *testing.T) {
	cases := []struct {
		name  string
		repo  string
		chart string
		want  string
	}{
		{
			name:  "short names are joined and hashed",
			repo:  "example",
			chart: "podinfo",
			want:  "example-chart-podinfo-aa661c3516b2",
		},
		{
			name:  "long names are truncated and suffixed with a hash",
			repo:  "yandex-cloud-marketplace-mirror",
			chart: "cert-manager-webhook-yandex",
			want:  "yandex-cloud-marketp-chart-cert-manager-webhook-a3ee4a8a584e",
		},
		{
			name:  "an empty chart name leaves no trailing dash before the hash",
			repo:  "repo",
			chart: "",
			want:  "repo-chart-4e5d8120682c",
		},
		{
			// A repository name is a DNS subdomain and a chart name comes from
			// the index, so either may carry a dot at the truncation boundary.
			name:  "a truncation that ends in a dot drops it",
			repo:  "abcdefghijklmnopqrs.x",
			chart: "podinfo",
			want:  "abcdefghijklmnopqrs-chart-podinfo-da22920998cb",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := HelmClusterAddonChartName(tc.repo, tc.chart); got != tc.want {
				t.Fatalf("HelmClusterAddonChartName(%q, %q) = %q, want %q", tc.repo, tc.chart, got, tc.want)
			}
		})
	}
}

func TestApplicationChartName(t *testing.T) {
	cases := []struct {
		name  string
		repo  string
		chart string
		want  string
	}{
		{
			name:  "short names are joined and hashed",
			repo:  "example",
			chart: "podinfo",
			want:  "example-chart-podinfo-aa661c3516b2",
		},
		{
			name:  "long names are truncated and suffixed with a hash",
			repo:  "yandex-cloud-marketplace-mirror",
			chart: "cert-manager-webhook-yandex",
			want:  "yandex-cloud-marketp-chart-cert-manager-webhook-a3ee4a8a584e",
		},
		{
			name:  "an empty chart name leaves no trailing dash before the hash",
			repo:  "repo",
			chart: "",
			want:  "repo-chart-4e5d8120682c",
		},
		{
			// A repository name is a DNS subdomain and a chart name comes from
			// the index, so either may carry a dot at the truncation boundary.
			name:  "a truncation that ends in a dot drops it",
			repo:  "abcdefghijklmnopqrs.x",
			chart: "podinfo",
			want:  "abcdefghijklmnopqrs-chart-podinfo-da22920998cb",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ApplicationChartName(tc.repo, tc.chart); got != tc.want {
				t.Fatalf("ApplicationChartName(%q, %q) = %q, want %q", tc.repo, tc.chart, got, tc.want)
			}
		})
	}
}

func TestClusterApplicationChartName(t *testing.T) {
	cases := []struct {
		name  string
		repo  string
		chart string
		want  string
	}{
		{
			name:  "short names are joined and hashed",
			repo:  "shared",
			chart: "nginx",
			want:  "shared-chart-nginx-7f9acafe347b",
		},
		{
			name:  "long names are truncated and suffixed with a hash",
			repo:  "yandex-cloud-marketplace-mirror",
			chart: "cert-manager-webhook-yandex",
			want:  "yandex-cloud-marketp-chart-cert-manager-webhook-a3ee4a8a584e",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClusterApplicationChartName(tc.repo, tc.chart); got != tc.want {
				t.Fatalf("ClusterApplicationChartName(%q, %q) = %q, want %q", tc.repo, tc.chart, got, tc.want)
			}
		})
	}
}

// TestChartNameSchemeIsShared pins the decision that every chart catalog kind is
// named by one scheme. The objects differ in kind, and the namespaced and cluster
// variants live in different scopes, so identical names cannot collide — while a
// scheme that silently diverged per family would break the round trip from an
// object name back to the repository/chart pair.
func TestChartNameSchemeIsShared(t *testing.T) {
	const (
		repo  = "yandex-cloud-marketplace-mirror"
		chart = "cert-manager-webhook-yandex"
	)

	addon := HelmClusterAddonChartName(repo, chart)

	if got := ApplicationChartName(repo, chart); got != addon {
		t.Fatalf("ApplicationChartName = %q, want the shared scheme result %q", got, addon)
	}

	if got := ClusterApplicationChartName(repo, chart); got != addon {
		t.Fatalf("ClusterApplicationChartName = %q, want the shared scheme result %q", got, addon)
	}
}

// TestHelmClusterAddonChartNameNoLongerCollidesOnATrailingDot pins that the addon
// family closed its collision gap too. Under its old scheme (a hash only past the
// truncation threshold), the two names below both trimmed to "foo-chart-bar" and
// collided: the trailing dot only vanishes from the untrimmed pair after it is
// already inside the joined string. The hash is computed from that untrimmed pair,
// so making it unconditional is what tells the two names apart now.
func TestHelmClusterAddonChartNameNoLongerCollidesOnATrailingDot(t *testing.T) {
	withDot := HelmClusterAddonChartName("foo", "bar.")
	withoutDot := HelmClusterAddonChartName("foo", "bar")

	if withDot == withoutDot {
		t.Fatalf("HelmClusterAddonChartName(%q, %q) = %q, collides with (%q, %q)", "foo", "bar.", withDot, "foo", "bar")
	}
}
