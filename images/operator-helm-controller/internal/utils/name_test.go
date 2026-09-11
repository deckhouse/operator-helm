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

package utils

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/util/validation"
)

// TestAddonInternalNamesAreFrozen pins the exact output of every addon naming
// function. These names are the names of live internal objects: changing one is
// a re-creation of a HelmRepository or a HelmRelease in every cluster running the
// module, so the values below must never change. The long input is 61 characters,
// past every truncation threshold the functions use.
func TestAddonInternalNamesAreFrozen(t *testing.T) {
	const long = "abcdefghijklmnopqrstuvwxyz-abcdefghijklmnopqrstuvwxyz-abcdefg"

	cases := []struct {
		name string
		fn   func(string) string
		in   string
		want string
	}{
		{"helm repository short", GetInternalHelmRepositoryName, "example", "hcar-example"},
		{"helm repository long", GetInternalHelmRepositoryName, long, "hcar-abcdefghijklmnopqrstuvwxyz-abcdefghijklmnopqr-ad1b0a084742"},
		{"auth secret short", GetInternalRepositoryAuthSecretName, "example", "hcar-auth-example"},
		{"auth secret long", GetInternalRepositoryAuthSecretName, long, "hcar-auth-abcdefghijklmnopqrstuvwxyz-abcdefghijklm-82ebac9f8734"},
		{"tls secret short", GetInternalRepositoryTLSSecretName, "example", "hcar-tls-example"},
		{"tls secret long", GetInternalRepositoryTLSSecretName, long, "hcar-tls-abcdefghijklmnopqrstuvwxyz-abcdefghijklmn-66c296d25f00"},
		{"helm release short", GetInternalHelmReleaseName, "example", "hca-example"},
		{"helm release long", GetInternalHelmReleaseName, long, "hca-abcdefghijklmnopqrstuvwxyz-abcdefghijklmnopqrs-e5ded5e7c79d"},
		{"helm chart equals release", GetInternalHelmChartName, long, GetInternalHelmReleaseName(long)},
		{"oci repository equals release", GetInternalOCIRepositoryName, long, GetInternalHelmReleaseName(long)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.fn(tc.in); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
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
			// Twin of TestApplicationRepositoryInternalName in
			// tests/e2e/internal/naming/naming_test.go: a change on either side
			// that is not mirrored on the other breaks one of the two tests.
			name:      "namespaced source carries namespace, name and a hash",
			prefix:    "hapr",
			kind:      "HelmApplicationRepository",
			namespace: "team-a",
			object:    "stable",
			want:      "hapr-team-a-stable-42df68033b1e",
		},
		{
			// Twin of TestApplicationServiceAccountName's first case in
			// tests/e2e/internal/naming/naming_test.go: a change on either side
			// that is not mirrored on the other breaks one of the two tests.
			name:      "twin of the e2e ApplicationServiceAccountName fixture",
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
			// A resource name is a DNS subdomain, so it may carry dots. Keeping
			// one at the cut would put the joining dash at the start of a label
			// and the API server would reject every object named this way.
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

// TestDerivedNameStaysWithinTheLabelLimitForTheLongestPrefix guards the arithmetic
// behind the 18-character part limit: the longest prefix any adapter uses is
// "hcapr-auth" (10 characters), and prefix + namespace + name + hash + three
// dashes must fit into 63.
func TestDerivedNameStaysWithinTheLabelLimitForTheLongestPrefix(t *testing.T) {
	got := DerivedName("hcapr-auth", "Kind", strings.Repeat("n", 40), strings.Repeat("m", 40))
	if len(got) > 63 {
		t.Fatalf("%q is %d characters, the limit is 63", got, len(got))
	}
}

// TestHelmReleaseName pins the release-name rule: Helm rejects names longer than 53
// characters. A name within the limit passes through untouched — every addon that
// exists today keeps its release — and a longer one is cut and suffixed with a hash
// of the full name so two long names sharing a prefix stay distinct.
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
			// Twin of the "a long name is cut to 40 characters and hashed" case in
			// TestApplicationReleaseName
			// (tests/e2e/internal/naming/naming_test.go): a change on either side
			// that is not mirrored on the other breaks one of the two tests.
			"hap-prefixed name over the limit is cut and hashed",
			"hap-very-long-application-name-that-is-definitely-over-fifty-three-characters-long",
			"hap-very-long-application-name-that-is-d-3080981cd4e1",
		},
		{
			// The hash is over the whole name, so two names that survive the cut
			// identically still get different releases. Without it the second
			// application would take over the first one's release.
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
			// A resource name may carry dots, and a cut landing on one would
			// leave the hash suffix starting a DNS label.
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
