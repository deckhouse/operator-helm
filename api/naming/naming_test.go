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

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/util/validation"
)

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
			want:  "example-chart-podinfo-015bdf9886f6",
		},
		{
			name:  "long names are truncated and suffixed with a hash",
			repo:  "yandex-cloud-marketplace-mirror",
			chart: "cert-manager-webhook-yandex",
			want:  "yandex-cloud-marketp-chart-cert-manager-webhook-cb0f7a51035d",
		},
		{
			name:  "an empty chart name leaves no trailing dash before the hash",
			repo:  "repo",
			chart: "",
			want:  "repo-chart-8549288388a9",
		},
		{
			name:  "a truncation that ends in a dot drops it",
			repo:  "abcdefghijklmnopqrs.x",
			chart: "podinfo",
			want:  "abcdefghijklmnopqrs-chart-podinfo-0fe4a214e986",
		},
		{
			name:  "an untruncated name ending in a dot still drops it",
			repo:  "abcdefghijklmnopqrs.",
			chart: "podinfo",
			want:  "abcdefghijklmnopqrs-chart-podinfo-b5579464eede",
		},
		{
			name:  "upper case is lowered",
			repo:  "REPO",
			chart: "CHART",
			want:  "repo-chart-chart-e5db4c98cda1",
		},
		{
			name:  "a space is replaced, not dropped, so the parts stay separated",
			repo:  "repo",
			chart: "ch art",
			want:  "repo-chart-ch-art-2e144a9bd47b",
		},
		{
			name:  "two empty parts still start with a letter, not a dash",
			repo:  "",
			chart: "",
			want:  "chart-6e340b9cffb3",
		},
		{
			name:  "separators in both parts stay single dashes",
			repo:  "ch art",
			chart: "x y z",
			want:  "ch-art-chart-x-y-z-2e9c09ddbba9",
		},
		{
			name:  "a dash next to a replaced dot collapses into one dash",
			repo:  "my chart. v2",
			chart: "a..b",
			want:  "my-chart-v2-chart-a-b-a0503e359778",
		},
		{
			name:  "parts made of separators only leave the bare literal",
			repo:  "   ",
			chart: "-.-",
			want:  "chart-9d95432984ec",
		},
		{
			name:  "long names with separators are truncated and collapsed",
			repo:  "a very long  repository  name here",
			chart: "an  extremely long chart  name here",
			want:  "a-very-long-reposit-chart-an-extremely-long-c-f348d6ed2e45",
		},
		{
			name:  "a dot between two alphanumerics is kept",
			repo:  "repo",
			chart: "a.b",
			want:  "repo-chart-a.b-ce05cc273120",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := HelmClusterAddonChartName(tc.repo, tc.chart)
			if got != tc.want {
				t.Fatalf("HelmClusterAddonChartName(%q, %q) = %q, want %q", tc.repo, tc.chart, got, tc.want)
			}
			assertNameIsValid(t, got)
		})
	}
}

func assertNameIsValid(t *testing.T, name string) {
	t.Helper()

	if errs := validation.IsDNS1123Subdomain(name); len(errs) > 0 {
		t.Fatalf("%q is not a valid DNS-1123 subdomain: %v", name, errs)
	}

	if len(name) > validation.DNS1123LabelMaxLength {
		t.Fatalf("%q is %d bytes, longer than a DNS-1123 label", name, len(name))
	}

	if strings.Contains(name, "--") {
		t.Fatalf("%q contains a run of dashes", name)
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
			want:  "example-chart-podinfo-015bdf9886f6",
		},
		{
			name:  "long names are truncated and suffixed with a hash",
			repo:  "yandex-cloud-marketplace-mirror",
			chart: "cert-manager-webhook-yandex",
			want:  "yandex-cloud-marketp-chart-cert-manager-webhook-cb0f7a51035d",
		},
		{
			name:  "an empty chart name leaves no trailing dash before the hash",
			repo:  "repo",
			chart: "",
			want:  "repo-chart-8549288388a9",
		},
		{
			name:  "a truncation that ends in a dot drops it",
			repo:  "abcdefghijklmnopqrs.x",
			chart: "podinfo",
			want:  "abcdefghijklmnopqrs-chart-podinfo-0fe4a214e986",
		},
		{
			name:  "an untruncated name ending in a dot still drops it",
			repo:  "abcdefghijklmnopqrs.",
			chart: "podinfo",
			want:  "abcdefghijklmnopqrs-chart-podinfo-b5579464eede",
		},
		{
			name:  "upper case is lowered",
			repo:  "REPO",
			chart: "CHART",
			want:  "repo-chart-chart-e5db4c98cda1",
		},
		{
			name:  "a space is replaced, not dropped, so the parts stay separated",
			repo:  "repo",
			chart: "ch art",
			want:  "repo-chart-ch-art-2e144a9bd47b",
		},
		{
			name:  "two empty parts still start with a letter, not a dash",
			repo:  "",
			chart: "",
			want:  "chart-6e340b9cffb3",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ApplicationChartName(tc.repo, tc.chart)
			if got != tc.want {
				t.Fatalf("ApplicationChartName(%q, %q) = %q, want %q", tc.repo, tc.chart, got, tc.want)
			}
			assertNameIsValid(t, got)
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
			want:  "shared-chart-nginx-a2f6f72110ff",
		},
		{
			name:  "long names are truncated and suffixed with a hash",
			repo:  "yandex-cloud-marketplace-mirror",
			chart: "cert-manager-webhook-yandex",
			want:  "yandex-cloud-marketp-chart-cert-manager-webhook-cb0f7a51035d",
		},
		{
			name:  "upper case is lowered",
			repo:  "REPO",
			chart: "CHART",
			want:  "repo-chart-chart-e5db4c98cda1",
		},
		{
			name:  "a space is replaced, not dropped, so the parts stay separated",
			repo:  "repo",
			chart: "ch art",
			want:  "repo-chart-ch-art-2e144a9bd47b",
		},
		{
			name:  "two empty parts still start with a letter, not a dash",
			repo:  "",
			chart: "",
			want:  "chart-6e340b9cffb3",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ClusterApplicationChartName(tc.repo, tc.chart)
			if got != tc.want {
				t.Fatalf("ClusterApplicationChartName(%q, %q) = %q, want %q", tc.repo, tc.chart, got, tc.want)
			}
			assertNameIsValid(t, got)
		})
	}
}

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

func TestChartNameSeparatesAnAmbiguousPair(t *testing.T) {
	left := HelmClusterAddonChartName("abc", "def-chart-ghi")
	right := HelmClusterAddonChartName("abc-chart-def", "ghi")

	if left == right {
		t.Fatalf("(%q, %q) and (%q, %q) both produce %q", "abc", "def-chart-ghi", "abc-chart-def", "ghi", left)
	}
}

func TestChartNameSeparatesATrailingDot(t *testing.T) {
	withDot := HelmClusterAddonChartName("foo", "bar.")
	withoutDot := HelmClusterAddonChartName("foo", "bar")

	if withDot == withoutDot {
		t.Fatalf("(%q, %q) and (%q, %q) both produce %q", "foo", "bar.", "foo", "bar", withDot)
	}
}

func TestChartNameSanitizesAnIllegalDot(t *testing.T) {
	charts := []string{
		"my chart. v2",
		".foo",
		"a..b",
		"a.-b",
		"foo.-bar",
		"名.app",
		"foo-.bar",
		".",
		"..",
	}

	for _, chart := range charts {
		t.Run(chart, func(t *testing.T) {
			assertNameIsValid(t, HelmClusterAddonChartName("repo", chart))
		})
	}
}

func TestCollapseDashes(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{name: "an empty string is left alone", in: "", want: ""},
		{name: "a single dash is kept", in: "-", want: "-"},
		{name: "a string of dashes becomes one", in: "-----", want: "-"},
		{name: "a leading run collapses", in: "--a", want: "-a"},
		{name: "a trailing run collapses", in: "a--", want: "a-"},
		{name: "runs of different lengths each collapse", in: "a--b---c----d", want: "a-b-c-d"},
		{name: "single dashes are untouched", in: "a-b-c", want: "a-b-c"},
		{name: "dots are not collapsed", in: "a..b", want: "a..b"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := collapseDashes(tc.in); got != tc.want {
				t.Fatalf("collapseDashes(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
