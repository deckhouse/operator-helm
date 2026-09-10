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
			name:      "namespaced source carries namespace, name and a hash",
			prefix:    "hapr",
			kind:      "HelmApplicationRepository",
			namespace: "team-a",
			object:    "stable",
			want:      "hapr-team-a-stable-42df68033b1e",
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
