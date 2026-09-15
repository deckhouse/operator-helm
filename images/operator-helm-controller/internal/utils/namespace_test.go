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

import "testing"

// TestIsSystemNamespace pins where an application may not be installed. The rule
// matters more than it did: until this family existed, only a cluster administrator
// could create anything namespaced here, and installing an application seeds a Role
// granting everything inside its namespace.
func TestIsSystemNamespace(t *testing.T) {
	cases := []struct {
		namespace string
		want      bool
	}{
		{"kube-system", true},
		{"kube-node-lease", true},
		{"kube-public", true},
		{"kube-anything", true},
		{"default", false},
		{"d8-operator-helm", true},
		{"d8-system", true},
		{"team-a", false},
		{"kubernetes-dashboard", false},
		{"defaults", false},
		{"", false},
	}

	for _, tc := range cases {
		t.Run(tc.namespace, func(t *testing.T) {
			if got := IsSystemNamespace(tc.namespace); got != tc.want {
				t.Fatalf("IsSystemNamespace(%q) = %v, want %v", tc.namespace, got, tc.want)
			}
		})
	}
}
