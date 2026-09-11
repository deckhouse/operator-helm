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

// TestApplicationServiceAccountName pins ApplicationServiceAccountName's output,
// including against the value operator-helm-controller's DerivedName produces for
// the same inputs.
//
// The first case is the twin of TestDerivedName's
// `DerivedName("hap", "HelmApplication", "e2e-app-ns", "e2e-test-app")` case in
// images/operator-helm-controller/internal/utils/name_test.go: a change on either
// side that is not mirrored on the other breaks one of the two tests.
func TestApplicationServiceAccountName(t *testing.T) {
	cases := []struct {
		name      string
		namespace string
		object    string
		want      string
	}{
		{
			name:      "twin of DerivedName(hap, HelmApplication, e2e-app-ns, e2e-test-app)",
			namespace: "e2e-app-ns",
			object:    "e2e-test-app",
			want:      "hap-e2e-app-ns-e2e-test-app-26155b312741",
		},
		{
			name:      "long parts are truncated to 18 characters each",
			namespace: "very-long-namespace-name-exceeding",
			object:    "very-long-application-name-exceeding",
			want:      "hap-very-long-namespac-very-long-applicat-35f137b7281e",
		},
		{
			name:      "a truncation that ends in a dash drops it",
			namespace: "abcdefghijklmnopq-x",
			object:    "stable",
			want:      "hap-abcdefghijklmnopq-stable-2b3b33a25854",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ApplicationServiceAccountName(tc.namespace, tc.object)
			if got != tc.want {
				t.Fatalf("ApplicationServiceAccountName(%q, %q) = %q, want %q", tc.namespace, tc.object, got, tc.want)
			}
		})
	}
}

// TestApplicationReleaseName pins ApplicationReleaseName's output, including
// against the value operator-helm-controller's HelmReleaseName produces for the
// same "hap-"-prefixed input.
func TestApplicationReleaseName(t *testing.T) {
	cases := []struct {
		name   string
		object string
		want   string
	}{
		{
			name:   "a short name is used as is",
			object: "e2e-test-app-helm",
			want:   "hap-e2e-test-app-helm",
		},
		{
			// Twin of the "hap-prefixed name over the limit is cut and hashed" case
			// in TestHelmReleaseName
			// (images/operator-helm-controller/internal/utils/name_test.go): a
			// change on either side that is not mirrored on the other breaks one of
			// the two tests.
			name:   "a long name is cut to 40 characters and hashed",
			object: "very-long-application-name-that-is-definitely-over-fifty-three-characters-long",
			want:   "hap-very-long-application-name-that-is-d-3080981cd4e1",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ApplicationReleaseName(tc.object)
			if got != tc.want {
				t.Fatalf("ApplicationReleaseName(%q) = %q, want %q", tc.object, got, tc.want)
			}
		})
	}
}
