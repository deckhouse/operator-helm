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

func TestAuxResourceNameReadableHints(t *testing.T) {
	name := AuxResourceName("GitHub", "Pod.Info", "6.7.1")
	if !strings.HasPrefix(name, "tmp-") {
		t.Fatalf("expected tmp- prefix, got %q", name)
	}
	if !strings.Contains(name, "github") || !strings.Contains(name, "pod-info") {
		t.Fatalf("expected sanitized repo/chart hints in %q", name)
	}
}

func TestAuxResourceNameDeterministic(t *testing.T) {
	a := AuxResourceName("github", "podinfo", "6.7.1")
	b := AuxResourceName("github", "podinfo", "6.7.1")
	if a != b {
		t.Fatalf("expected deterministic name, got %q and %q", a, b)
	}
}

func TestAuxResourceNameDistinct(t *testing.T) {
	cases := [][3]string{
		{"github", "podinfo", "6.7.1"},
		{"github", "podinfo", "6.7.2"},
		{"github", "nginx", "6.7.1"},
		{"gitlab", "podinfo", "6.7.1"},
	}

	seen := map[string]bool{}
	for _, c := range cases {
		name := AuxResourceName(c[0], c[1], c[2])
		if seen[name] {
			t.Fatalf("name collision for %v: %q", c, name)
		}
		seen[name] = true
	}
}

func TestAuxResourceNameValidDNS1123(t *testing.T) {
	name := AuxResourceName("really-long-repository-name", "really-long-chart-name", "1.2.3-alpha.1+build")
	if len(name) > 63 {
		t.Fatalf("name too long (%d): %q", len(name), name)
	}
	if !dns1123.MatchString(name) {
		t.Fatalf("name is not a valid DNS-1123 label: %q", name)
	}
}
