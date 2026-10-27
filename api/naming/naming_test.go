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

func TestHelmApplicationChartName(t *testing.T) {
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
			got := HelmApplicationChartName(tc.repo, tc.chart)
			if got != tc.want {
				t.Fatalf("HelmApplicationChartName(%q, %q) = %q, want %q", tc.repo, tc.chart, got, tc.want)
			}
			assertNameIsValid(t, got)
		})
	}
}

func TestHelmClusterApplicationChartName(t *testing.T) {
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
			got := HelmClusterApplicationChartName(tc.repo, tc.chart)
			if got != tc.want {
				t.Fatalf("HelmClusterApplicationChartName(%q, %q) = %q, want %q", tc.repo, tc.chart, got, tc.want)
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

	if got := HelmApplicationChartName(repo, chart); got != addon {
		t.Fatalf("HelmApplicationChartName = %q, want the shared scheme result %q", got, addon)
	}

	if got := HelmClusterApplicationChartName(repo, chart); got != addon {
		t.Fatalf("HelmClusterApplicationChartName = %q, want the shared scheme result %q", got, addon)
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

func TestDerivedName(t *testing.T) {
	cases := []struct {
		name      string
		prefix    string
		kind      string
		namespace string
		object    string
		want      string
	}{
		{
			name:      "namespaced source carries namespace, name and a hash",
			prefix:    "hapr",
			kind:      "HelmApplicationRepository",
			namespace: "team-a",
			object:    "stable",
			want:      "hapr-team-a-stable-42df68033b1e",
		},
		{
			name:      "an application in the namespace the e2e suite uses",
			prefix:    "hap",
			kind:      "HelmApplication",
			namespace: "e2e-app-ns",
			object:    "e2e-test-app",
			want:      "hap-e2e-app-ns-e2e-test-app-26155b312741",
		},
		{
			name:      "same name in another namespace is a different object",
			prefix:    "hapr",
			kind:      "HelmApplicationRepository",
			namespace: "team-b",
			object:    "stable",
			want:      "hapr-team-b-stable-20b1eaa1620a",
		},
		{
			name:      "cluster source has no namespace part",
			prefix:    "hcapr",
			kind:      "HelmClusterApplicationRepository",
			namespace: "",
			object:    "stable",
			want:      "hcapr-stable-63c0be2c8873",
		},
		{
			name:      "long parts are truncated to 18 characters each",
			prefix:    "hapr",
			kind:      "HelmApplicationRepository",
			namespace: "very-long-namespace-name-exceeding",
			object:    "very-long-repository-name-exceeding",
			want:      "hapr-very-long-namespac-very-long-reposito-355aadb4a138",
		},
		{
			name:      "a truncation that ends in a dash drops it",
			prefix:    "hapr",
			kind:      "HelmApplicationRepository",
			namespace: "abcdefghijklmnopq-x",
			object:    "stable",
			want:      "hapr-abcdefghijklmnopq-stable-1846a7b21e01",
		},
		{
			name:      "long application parts are truncated to 18 characters each",
			prefix:    "hap",
			kind:      "HelmApplication",
			namespace: "very-long-namespace-name-exceeding",
			object:    "very-long-application-name-exceeding",
			want:      "hap-very-long-namespac-very-long-applicat-35f137b7281e",
		},
		{
			name:      "an application truncation that ends in a dash drops it",
			prefix:    "hap",
			kind:      "HelmApplication",
			namespace: "abcdefghijklmnopq-x",
			object:    "stable",
			want:      "hap-abcdefghijklmnopq-stable-2b3b33a25854",
		},
		{
			name:      "a truncation that ends in a dot drops it",
			prefix:    "hap",
			kind:      "HelmApplication",
			namespace: "team-a",
			object:    "abcdefghijklmnopq.x",
			want:      "hap-team-a-abcdefghijklmnopq-da2ee07a8439",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := DerivedName(tc.prefix, tc.kind, tc.namespace, tc.object)
			if got != tc.want {
				t.Fatalf("DerivedName(%q, %q, %q, %q) = %q, want %q", tc.prefix, tc.kind, tc.namespace, tc.object, got, tc.want)
			}
			if len(got) > 63 {
				t.Fatalf("%q is %d characters, the limit is 63", got, len(got))
			}
			if errs := validation.IsDNS1123Subdomain(got); len(errs) > 0 {
				t.Fatalf("%q is not a valid object name: %v", got, errs)
			}
		})
	}
}

func TestDerivedNameStaysWithinTheLabelLimitForTheLongestPrefix(t *testing.T) {
	got := DerivedName("hcapr-auth", "Kind", strings.Repeat("n", 40), strings.Repeat("m", 40))
	if len(got) > 63 {
		t.Fatalf("%q is %d characters, the limit is 63", got, len(got))
	}
}

func TestHashedReleaseNameSeparatesTwoValidNames(t *testing.T) {
	const (
		long  = "hap-abcdefghijklmnopqrstuvwxyz-abcdefghijklmnopqrstuvwxyz-abcdefg"
		short = "hap-abcdefghijklmnopqrstuvwxyz-abcdefghi-dbe791f54fec"
	)

	if HelmReleaseName(long) != HelmReleaseName(short) {
		t.Fatal("the fixture no longer demonstrates the collision it was chosen for")
	}

	if got, other := HashedReleaseName(long), HashedReleaseName(short); got == other {
		t.Fatalf("both names produce %q", got)
	}

	for _, name := range []string{long, short, "hap-a"} {
		got := HashedReleaseName(name)
		if len(got) > helmReleaseNameLimit {
			t.Fatalf("%q is %d characters, Helm accepts at most %d", got, len(got), helmReleaseNameLimit)
		}
		if errs := validation.IsDNS1123Subdomain(got); len(errs) > 0 {
			t.Fatalf("%q is not a valid release name: %v", got, errs)
		}
	}

	if got := HashedReleaseName("hap-e2e-test-app-helm"); got != "hap-e2e-test-app-helm-eaa08759b576" {
		t.Fatalf("HashedReleaseName = %q", got)
	}
}

func TestHashedReleaseNameCutsALongName(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "a long name is cut to 40 characters and hashed",
			in:   "hap-very-long-application-name-that-is-definitely-over-fifty-three-characters-long",
			want: "hap-very-long-application-name-that-is-d-3080981cd4e1",
		},
		{
			name: "a cut that lands on a dot drops it",
			in:   "hap-very.long.application.name.over.the.helm.limit",
			want: "hap-very.long.application.name.over.the-5f257e85dfb0",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := HashedReleaseName(tc.in)
			if got != tc.want {
				t.Fatalf("HashedReleaseName(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if len(got) > helmReleaseNameLimit {
				t.Fatalf("%q is %d characters, Helm accepts at most %d", got, len(got), helmReleaseNameLimit)
			}
			if errs := validation.IsDNS1123Subdomain(got); len(errs) > 0 {
				t.Fatalf("%q is not a valid release name: %v", got, errs)
			}
		})
	}
}

func TestHelmReleaseName(t *testing.T) {
	const long = "abcdefghijklmnopqrstuvwxyz-abcdefghijklmnopqrstuvwxyz-abcdefg"

	cases := []struct {
		name string
		in   string
		want string
	}{
		{"short name is used as is", "podinfo", "podinfo"},
		{"a name of exactly 53 characters is used as is", strings.Repeat("a", 53), strings.Repeat("a", 53)},
		{"a longer name is cut to 40 characters and hashed", long, "abcdefghijklmnopqrstuvwxyz-abcdefghijklm-ddc3f43e8c75"},
		{
			"a long name is distinguished by the hash, not by the cut",
			strings.Repeat("c", 50) + "-one",
			strings.Repeat("c", 40) + "-0511f1bf7dbf",
		},
		{
			"a name sharing the first 40 characters gets a different release",
			strings.Repeat("c", 50) + "-two",
			strings.Repeat("c", 40) + "-bbd09a562ff3",
		},
		{
			"a cut that lands on a dot drops it",
			strings.Repeat("a", 39) + "." + strings.Repeat("b", 20),
			strings.Repeat("a", 39) + "-b46d196cb11f",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := HelmReleaseName(tc.in)
			if got != tc.want {
				t.Fatalf("HelmReleaseName(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if len(got) > 53 {
				t.Fatalf("%q is %d characters, Helm accepts at most 53", got, len(got))
			}
			if errs := validation.IsDNS1123Subdomain(got); len(errs) > 0 {
				t.Fatalf("%q is not a valid release name: %v", got, errs)
			}
		})
	}
}

func TestApplicationFamilyNamesMatchTheE2EGoldenValues(t *testing.T) {
	cases := []struct {
		name string
		got  string
		want string
	}{
		{"application service account", DerivedName(ApplicationPrefix, "HelmApplication", "e2e-app-ns", "e2e-test-app"), "hap-e2e-app-ns-e2e-test-app-26155b312741"},
		{"application with long parts", DerivedName(ApplicationPrefix, "HelmApplication", "very-long-namespace-name-exceeding", "very-long-application-name-exceeding"), "hap-very-long-namespac-very-long-applicat-35f137b7281e"},
		{"application truncated on a dash", DerivedName(ApplicationPrefix, "HelmApplication", "abcdefghijklmnopq-x", "stable"), "hap-abcdefghijklmnopq-stable-2b3b33a25854"},
		{"application truncated on a dot", DerivedName(ApplicationPrefix, "HelmApplication", "team-a", "abcdefghijklmnopq.x"), "hap-team-a-abcdefghijklmnopq-da2ee07a8439"},
		{"application release", HashedReleaseName(ApplicationPrefix + "-" + "e2e-test-app-helm"), "hap-e2e-test-app-helm-eaa08759b576"},
		{"long application release", HashedReleaseName(ApplicationPrefix + "-" + "very-long-application-name-that-is-definitely-over-fifty-three-characters-long"), "hap-very-long-application-name-that-is-d-3080981cd4e1"},
		{"application repository", DerivedName(ApplicationRepositoryPrefix, "HelmApplicationRepository", "team-a", "stable"), "hapr-team-a-stable-42df68033b1e"},
		{"application repository with long parts", DerivedName(ApplicationRepositoryPrefix, "HelmApplicationRepository", "very-long-namespace-name-exceeding", "very-long-repository-name-exceeding"), "hapr-very-long-namespac-very-long-reposito-355aadb4a138"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.got != tc.want {
				t.Fatalf("got %q, want %q", tc.got, tc.want)
			}
		})
	}
}
