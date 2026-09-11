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
	// testKind is lower case because Resolve lower-cases the kind before it ever
	// reaches AuxResourceName, so that is the only casing production hashes.
	testKind = "helmclusteraddonrepository"
	// nsKind is a namespaced repository kind; its names must additionally depend
	// on the namespace.
	nsKind = "helmapplicationrepository"
)

func TestAuxResourceNameReadableHints(t *testing.T) {
	name := AuxResourceName(testKind, "", "GitHub", "Pod.Info", "6.7.1")
	if !strings.HasPrefix(name, "tmp-") {
		t.Fatalf("expected tmp- prefix, got %q", name)
	}
	if !strings.Contains(name, "github") || !strings.Contains(name, "pod-info") {
		t.Fatalf("expected sanitized repo/chart hints in %q", name)
	}
}

func TestAuxResourceNameDeterministic(t *testing.T) {
	a := AuxResourceName(testKind, "", "github", "podinfo", "6.7.1")
	b := AuxResourceName(testKind, "", "github", "podinfo", "6.7.1")
	if a != b {
		t.Fatalf("expected deterministic name, got %q and %q", a, b)
	}
}

func TestAuxResourceNameDistinct(t *testing.T) {
	cases := [][5]string{
		{testKind, "", "github", "podinfo", "6.7.1"},
		{testKind, "", "github", "podinfo", "6.7.2"},
		{testKind, "", "github", "nginx", "6.7.1"},
		{testKind, "", "gitlab", "podinfo", "6.7.1"},
		{"FutureRepository", "", "github", "podinfo", "6.7.1"}, // same name/chart/version, different kind
		{nsKind, "team-a", "stable", "podinfo", "6.7.1"},
		{nsKind, "team-b", "stable", "podinfo", "6.7.1"}, // same repository name in another namespace
	}

	seen := map[string]bool{}
	for _, c := range cases {
		name := AuxResourceName(c[0], c[1], c[2], c[3], c[4])
		if seen[name] {
			t.Fatalf("name collision for %v: %q", c, name)
		}
		seen[name] = true
	}
}

func TestAuxResourceNameValidDNS1123(t *testing.T) {
	name := AuxResourceName(testKind, "", "really-long-repository-name", "really-long-chart-name", "1.2.3-alpha.1+build")
	if len(name) > 63 {
		t.Fatalf("name too long (%d): %q", len(name), name)
	}
	if !dns1123.MatchString(name) {
		t.Fatalf("name is not a valid DNS-1123 label: %q", name)
	}
}

// TestAuxResourceNameClusterScopedNamesAreFrozen pins the names the addon family
// already uses: a cluster-scoped repository passes an empty namespace, and its name
// must not move — the objects are live and shared by every polling client.
func TestAuxResourceNameClusterScopedNamesAreFrozen(t *testing.T) {
	cases := []struct {
		repository string
		chart      string
		version    string
		want       string
	}{
		{"github", "podinfo", "6.7.1", "tmp-github-podinfo-1379a792462c3a85"},
		{"GitHub", "Pod.Info", "6.7.1", "tmp-github-pod-info-4aaba5a5ae371dec"},
	}

	for _, tc := range cases {
		got := AuxResourceName(testKind, "", tc.repository, tc.chart, tc.version)
		if got != tc.want {
			t.Fatalf("AuxResourceName(%q, \"\", %q, %q, %q) = %q, want %q",
				testKind, tc.repository, tc.chart, tc.version, got, tc.want)
		}
	}
}

// TestAuxResourceNameCarriesTheNamespaceHint keeps a namespaced name diagnosable:
// the namespace appears in the readable part, not only in the hash.
func TestAuxResourceNameCarriesTheNamespaceHint(t *testing.T) {
	name := AuxResourceName(nsKind, "team-a", "stable", "podinfo", "6.7.1")

	if !strings.Contains(name, "team-a") {
		t.Fatalf("namespaced name %q must carry the namespace hint", name)
	}
	if !dns1123.MatchString(name) || len(name) > 63 {
		t.Fatalf("name %q is not a valid DNS-1123 label of at most 63 characters", name)
	}
}
