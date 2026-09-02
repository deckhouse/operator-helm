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

func TestHelmClusterAddonChartName(t *testing.T) {
	cases := []struct {
		name  string
		repo  string
		chart string
		want  string
	}{
		{
			name:  "short names are joined verbatim",
			repo:  "example",
			chart: "podinfo",
			want:  "example-chart-podinfo",
		},
		{
			name:  "long names are truncated and suffixed with a hash",
			repo:  "yandex-cloud-marketplace-mirror",
			chart: "cert-manager-webhook-yandex",
			want:  "yandex-cloud-marketp-chart-cert-manager-webhook-a3ee4a8a584e",
		},
		{
			name:  "an empty chart name leaves no trailing dash",
			repo:  "repo",
			chart: "",
			want:  "repo-chart",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := HelmClusterAddonChartName(tc.repo, tc.chart); got != tc.want {
				t.Fatalf("HelmClusterAddonChartName(%q, %q) = %q, want %q", tc.repo, tc.chart, got, tc.want)
			}
		})
	}
}
