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

package catalog

import (
	"testing"

	"github.com/Masterminds/semver/v3"

	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
	repoclient "github.com/deckhouse/operator-helm/internal/client/repository"
)

// TestMergeChartVersionsCarriesOCIRef pins the three things that can happen to a
// recorded reference. Fresh index data always wins, which is how a version
// re-published as an archive loses its reference; a version the index no longer
// offers keeps it, without which the addon still using it could not build its
// internal OCIRepository and would be blocked from every change, including its own
// removal.
func TestMergeChartVersionsCarriesOCIRef(t *testing.T) {
	fetched := []repoclient.ChartVersion{
		{Version: semver.MustParse("3.0.0"), OCIRef: "oci://registry.example.com/charts/podinfo:3.0.0"},
		{Version: semver.MustParse("2.0.0")},
	}

	current := []helmv1alpha1.ChartVersion{
		{Version: "2.0.0", OCIRef: "oci://registry.example.com/charts/podinfo:2.0.0"},
		{Version: "1.0.0", OCIRef: "oci://registry.example.com/charts/podinfo:1.0.0"},
	}

	inUse := map[string]struct{}{"1.0.0": {}}

	merged := mergeChartVersions(fetched, current, inUse)

	byVersion := map[string]helmv1alpha1.ChartVersion{}
	for _, version := range merged {
		byVersion[version.Version] = version
	}

	if got := byVersion["3.0.0"].OCIRef; got != "oci://registry.example.com/charts/podinfo:3.0.0" {
		t.Fatalf("3.0.0 oci ref = %q, want the fetched one", got)
	}
	if got := byVersion["2.0.0"].OCIRef; got != "" {
		t.Fatalf("2.0.0 oci ref = %q, want empty: the index now offers an archive", got)
	}

	retained, ok := byVersion["1.0.0"]
	if !ok {
		t.Fatal("a version still referenced by an addon must be retained")
	}
	if retained.OCIRef != "oci://registry.example.com/charts/podinfo:1.0.0" {
		t.Fatalf("retained oci ref = %q, want the recorded one", retained.OCIRef)
	}
	if retained.UnavailableReason != helmv1alpha1.UnavailableReasonRemovedFromRepository {
		t.Fatalf("retained reason = %q, want %q", retained.UnavailableReason, helmv1alpha1.UnavailableReasonRemovedFromRepository)
	}
}

// TestMergeChartVersionsDoesNotCarryMediaTypeOntoOCIRef pins the invariant the API
// documentation asserts: MediaType stays empty for a version carrying OCIRef. A
// version that now resolves to an OCI artifact must probe its own layer media type
// from scratch even though an addon still references it and a previous pass (back
// when the version was an archive) recorded one: resolveMediaType checks
// version.MediaType != "" before the force-reconcile cache bypass, so a stale
// carried-forward value would use the wrong layer selector and no force reconcile
// could ever correct it.
func TestMergeChartVersionsDoesNotCarryMediaTypeOntoOCIRef(t *testing.T) {
	fetched := []repoclient.ChartVersion{
		{Version: semver.MustParse("6.7.1"), OCIRef: "oci://other-registry.example.com/x/podinfo:6.7.1"},
	}

	current := []helmv1alpha1.ChartVersion{
		{Version: "6.7.1", MediaType: "application/tar+gzip"},
	}

	inUse := map[string]struct{}{"6.7.1": {}}

	merged := mergeChartVersions(fetched, current, inUse)

	if len(merged) != 1 {
		t.Fatalf("merged = %+v, want exactly one version", merged)
	}
	if merged[0].OCIRef != "oci://other-registry.example.com/x/podinfo:6.7.1" {
		t.Fatalf("OCIRef = %q, want the fetched one", merged[0].OCIRef)
	}
	if merged[0].MediaType != "" {
		t.Fatalf("MediaType = %q, want empty: a version carrying OCIRef must not carry a stale media type forward", merged[0].MediaType)
	}
}
