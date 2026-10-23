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
	"regexp"
	"strings"
	"testing"
)

var dns1123 = regexp.MustCompile(`^[a-z]([-a-z0-9]*[a-z0-9])?$`)

const (
	testKind = "helmclusteraddonrepository"
	nsKind   = "helmapplicationrepository"

	testIdentity = "0f1e2d3c-4b5a-6978-8796-a5b4c3d2e1f0@1"
)

func TestAuxResourceNameReadableHints(t *testing.T) {
	name := AuxResourceName(testKind, "", "GitHub", "Pod.Info", "6.7.1", testIdentity)
	if !strings.HasPrefix(name, "tmp-") {
		t.Fatalf("expected tmp- prefix, got %q", name)
	}
	if !strings.Contains(name, "github") || !strings.Contains(name, "pod-info") {
		t.Fatalf("expected sanitized repo/chart hints in %q", name)
	}
}

func TestAuxResourceNameDeterministic(t *testing.T) {
	a := AuxResourceName(testKind, "", "github", "podinfo", "6.7.1", testIdentity)
	b := AuxResourceName(testKind, "", "github", "podinfo", "6.7.1", testIdentity)
	if a != b {
		t.Fatalf("expected deterministic name, got %q and %q", a, b)
	}
}

func TestAuxResourceNameDistinct(t *testing.T) {
	cases := [][6]string{
		{testKind, "", "github", "podinfo", "6.7.1", testIdentity},
		{testKind, "", "github", "podinfo", "6.7.2", testIdentity},
		{testKind, "", "github", "nginx", "6.7.1", testIdentity},
		{testKind, "", "gitlab", "podinfo", "6.7.1", testIdentity},
		{"FutureRepository", "", "github", "podinfo", "6.7.1", testIdentity},
		{nsKind, "team-a", "stable", "podinfo", "6.7.1", testIdentity},
		{nsKind, "team-b", "stable", "podinfo", "6.7.1", testIdentity},
		{testKind, "", "github", "podinfo", "6.7.1", "11111111-2222-3333-4444-555555555555@1"},
		{testKind, "", "github", "podinfo", "6.7.1", "0f1e2d3c-4b5a-6978-8796-a5b4c3d2e1f0@2"},
		{nsKind, "team-a", "stable", "podinfo", "6.7.1", "11111111-2222-3333-4444-555555555555@1"},
	}

	seen := map[string]bool{}
	for _, c := range cases {
		name := AuxResourceName(c[0], c[1], c[2], c[3], c[4], c[5])
		if seen[name] {
			t.Fatalf("name collision for %v: %q", c, name)
		}
		seen[name] = true
	}
}

func TestAuxResourceNameValidDNS1123(t *testing.T) {
	name := AuxResourceName(testKind, "", "really-long-repository-name", "really-long-chart-name", "1.2.3-alpha.1+build", testIdentity)
	if len(name) > 63 {
		t.Fatalf("name too long (%d): %q", len(name), name)
	}
	if !dns1123.MatchString(name) {
		t.Fatalf("name is not a valid DNS-1123 label: %q", name)
	}
}

func TestAuxResourceNameClusterScopedNamesAreFrozen(t *testing.T) {
	cases := []struct {
		repository string
		chart      string
		version    string
		want       string
	}{
		{"github", "podinfo", "6.7.1", "tmp-github-podinfo-f5a6fa0f81e9183a"},
		{"GitHub", "Pod.Info", "6.7.1", "tmp-github-pod-info-a36ce0686457656a"},
	}

	for _, tc := range cases {
		got := AuxResourceName(testKind, "", tc.repository, tc.chart, tc.version, testIdentity)
		if got != tc.want {
			t.Fatalf("AuxResourceName(%q, \"\", %q, %q, %q, %q) = %q, want %q",
				testKind, tc.repository, tc.chart, tc.version, testIdentity, got, tc.want)
		}
	}
}

func TestAuxResourceNameKeepsTheHintsAcrossIdentities(t *testing.T) {
	before := AuxResourceName(testKind, "", "github", "podinfo", "6.7.1", testIdentity)
	after := AuxResourceName(testKind, "", "github", "podinfo", "6.7.1", "11111111-2222-3333-4444-555555555555@1")

	if before == after {
		t.Fatal("a repository recreated under the same name must not reuse the auxiliary object of its predecessor")
	}

	const hints = "tmp-github-podinfo-"
	if !strings.HasPrefix(before, hints) || !strings.HasPrefix(after, hints) {
		t.Fatalf("names %q and %q must both carry the repository and chart hints", before, after)
	}
}

func TestAuxResourceNameCarriesTheNamespaceHint(t *testing.T) {
	name := AuxResourceName(nsKind, "team-a", "stable", "podinfo", "6.7.1", testIdentity)

	if !strings.Contains(name, "team-a") {
		t.Fatalf("namespaced name %q must carry the namespace hint", name)
	}
	if !dns1123.MatchString(name) || len(name) > 63 {
		t.Fatalf("name %q is not a valid DNS-1123 label of at most 63 characters", name)
	}
}
