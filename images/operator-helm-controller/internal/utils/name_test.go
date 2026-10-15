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
		{
			"chart claim lease",
			func(chart string) string { return GetChartClaimLeaseName("example", chart) },
			"podinfo",
			"hca-claim-eb0e2bd49680",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.fn(tc.in); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}
