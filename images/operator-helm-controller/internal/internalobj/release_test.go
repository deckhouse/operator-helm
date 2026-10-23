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

package internalobj

import (
	"context"
	"testing"

	helmv2 "github.com/fluxcd/helm-controller/api/v2"
	sourcev1 "github.com/fluxcd/source-controller/api/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/deckhouse/operator-helm/internal/adapter"
	"github.com/deckhouse/operator-helm/internal/chartsource"
	"github.com/deckhouse/operator-helm/internal/contract"
)

func newReleases(t *testing.T, objects ...client.Object) (*Releases, client.Client) {
	t.Helper()

	scheme := testScheme(t)
	for _, add := range []func(*runtime.Scheme) error{
		sourcev1.AddToScheme,
		helmv2.AddToScheme,
	} {
		if err := add(scheme); err != nil {
			t.Fatalf("registering scheme: %v", err)
		}
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()

	return NewReleases(c, testNamespace), c
}

func ensureRelease(t *testing.T, service *Releases, c client.Client, rel contract.Release) *helmv2.HelmRelease {
	t.Helper()

	if res := service.Ensure(context.Background(), rel, chartsource.Helm, "", true); res.Err != nil {
		t.Fatalf("Ensure returned %v", res.Err)
	}

	release := &helmv2.HelmRelease{}
	key := client.ObjectKey{Namespace: testNamespace, Name: rel.InternalNames().HelmRelease}
	if err := c.Get(context.Background(), key, release); err != nil {
		t.Fatalf("helm release was not created: %v", err)
	}

	return release
}

func TestReleasesEnsureAppliesAnApplicationAsItsOwnIdentity(t *testing.T) {
	rel := adapter.NewApplicationRelease(testApplication())
	service, c := newReleases(t)

	release := ensureRelease(t, service, c, rel)

	if want := rel.InternalNames().ServiceAccount; release.Spec.ServiceAccountName != want {
		t.Fatalf("serviceAccountName = %q, want %q", release.Spec.ServiceAccountName, want)
	}
	if release.Spec.StorageNamespace != "team-a" {
		t.Fatalf("storageNamespace = %q, want the application namespace", release.Spec.StorageNamespace)
	}
}

func TestReleasesEnsureLeavesTheAddonIdentityAlone(t *testing.T) {
	rel := adapter.NewAddonRelease(testAddon())
	service, c := newReleases(t)

	release := ensureRelease(t, service, c, rel)

	if release.Spec.ServiceAccountName != "" {
		t.Fatalf("serviceAccountName = %q, want it unset", release.Spec.ServiceAccountName)
	}
	if release.Spec.StorageNamespace != "" {
		t.Fatalf("storageNamespace = %q, want it unset", release.Spec.StorageNamespace)
	}
}

func TestReleasesEnsureKeepsForeignLabels(t *testing.T) {
	rel := adapter.NewApplicationRelease(testApplication())
	existing := &helmv2.HelmRelease{
		ObjectMeta: metav1.ObjectMeta{
			Name:      rel.InternalNames().HelmRelease,
			Namespace: testNamespace,
			Labels:    map[string]string{"cost-center": "team-a"},
		},
	}
	service, c := newReleases(t, existing)

	release := ensureRelease(t, service, c, rel)

	if release.Labels["cost-center"] != "team-a" {
		t.Fatalf("labels = %v, want the foreign label preserved", release.Labels)
	}
	for key, want := range rel.SourceLabels() {
		if release.Labels[key] != want {
			t.Fatalf("label %q = %q, want %q", key, release.Labels[key], want)
		}
	}
}

func TestIsDesiredChartDeployedIdentifiesAnOCIChartByItsDigest(t *testing.T) {
	const digest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

	cases := []struct {
		name             string
		version          string
		artifactRevision string
		status           string
		snapshot         helmv2.Snapshot
		want             bool
	}{
		{
			name:             "a tag spelled differently from the chart version",
			version:          "v6.7.1",
			artifactRevision: "v6.7.1@" + digest,
			status:           "deployed",
			snapshot:         helmv2.Snapshot{OCIDigest: digest, ChartVersion: "6.7.1+0123456789ab"},
			want:             true,
		},
		{
			name:             "a tag equal to the chart version",
			version:          "6.7.1",
			artifactRevision: "6.7.1@" + digest,
			status:           "deployed",
			snapshot:         helmv2.Snapshot{OCIDigest: digest, ChartVersion: "6.7.1+0123456789ab"},
			want:             true,
		},
		{
			name:             "a recorded digest that kept the tag in front of it",
			version:          "stable",
			artifactRevision: "stable@" + digest,
			status:           "deployed",
			snapshot:         helmv2.Snapshot{OCIDigest: "stable@" + digest, ChartVersion: "6.7.1+0123456789ab"},
			want:             true,
		},
		{
			name:             "another artifact under the same tag",
			version:          "6.7.1",
			artifactRevision: "6.7.1@sha256:fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210",
			status:           "deployed",
			snapshot:         helmv2.Snapshot{OCIDigest: digest, ChartVersion: "6.7.1+0123456789ab"},
			want:             false,
		},
		{
			name:             "the artifact is deployed but the release failed",
			version:          "6.7.1",
			artifactRevision: "6.7.1@" + digest,
			status:           "failed",
			snapshot:         helmv2.Snapshot{OCIDigest: digest, ChartVersion: "6.7.1+0123456789ab"},
			want:             false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			app := testApplication()
			app.Spec.Chart.Version = tc.version
			rel := adapter.NewApplicationRelease(app)

			latest := tc.snapshot
			latest.Status = tc.status

			if got := isDesiredChartDeployed(rel, &latest, tc.artifactRevision); got != tc.want {
				t.Fatalf("isDesiredChartDeployed(_, %+v, %q) = %v, want %v",
					latest, tc.artifactRevision, got, tc.want)
			}
		})
	}
}

func TestIsDesiredChartDeployedComparesVersionsForAnArchiveChart(t *testing.T) {
	cases := []struct {
		name    string
		version string
		latest  helmv2.Snapshot
		want    bool
	}{
		{
			name:    "the deployed version is the one asked for",
			version: "6.7.1",
			latest:  helmv2.Snapshot{Status: "deployed", ChartVersion: "6.7.1"},
			want:    true,
		},
		{
			name:    "another version is deployed",
			version: "6.7.1",
			latest:  helmv2.Snapshot{Status: "deployed", ChartVersion: "6.7.0"},
			want:    false,
		},
		{
			name:    "the release asks for no version at all",
			version: "",
			latest:  helmv2.Snapshot{Status: "deployed", ChartVersion: "6.7.1"},
			want:    false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			app := testApplication()
			app.Spec.Chart.Version = tc.version
			rel := adapter.NewApplicationRelease(app)

			if got := isDesiredChartDeployed(rel, &tc.latest, "6.7.1"); got != tc.want {
				t.Fatalf("isDesiredChartDeployed(_, %+v, _) = %v, want %v", tc.latest, got, tc.want)
			}
		})
	}
}

func TestIsDesiredChartDeployedSurvivesASourceKindFlip(t *testing.T) {
	rel := adapter.NewApplicationRelease(testApplication())

	cases := []struct {
		name             string
		artifactRevision string
	}{
		{"a helm chart revision carries no digest", "6.7.1"},
		{"an empty revision", ""},
		{"a revision whose digest is not the recorded one", "6.7.1@sha256:ab"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			latest := &helmv2.Snapshot{
				Status:       "deployed",
				OCIDigest:    "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
				ChartVersion: "6.7.1+0123456789ab",
			}

			if isDesiredChartDeployed(rel, latest, tc.artifactRevision) {
				t.Fatalf("isDesiredChartDeployed(_, _, %q) = true, want false", tc.artifactRevision)
			}
		})
	}
}

func TestReleasesEnsureReportsNothingDeployedFromAnArtifactOfAnotherSpec(t *testing.T) {
	const digest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

	app := testApplication()
	app.Spec.Chart.Version = "6.7.1"
	rel := adapter.NewApplicationRelease(app)

	internal := &helmv2.HelmRelease{
		ObjectMeta: metav1.ObjectMeta{
			Name:       rel.InternalNames().HelmRelease,
			Namespace:  testNamespace,
			Generation: 1,
		},
		Status: helmv2.HelmReleaseStatus{
			History: helmv2.Snapshots{{
				Status:       "deployed",
				OCIDigest:    digest,
				ChartVersion: "6.7.1+0123456789ab",
			}},
		},
	}

	for _, tc := range []struct {
		name    string
		current bool
		want    bool
	}{
		{"the artifact is the one this spec asks for", true, true},
		{"the artifact is what the source kept after failing this spec", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			service, _ := newReleases(t, internal.DeepCopy())

			res := service.Ensure(context.Background(), rel, chartsource.OCI, "6.7.1@"+digest, tc.current)
			if res.Err != nil {
				t.Fatalf("Ensure returned %v", res.Err)
			}
			if res.ChartDeployed != tc.want {
				t.Fatalf("ChartDeployed = %v, want %v", res.ChartDeployed, tc.want)
			}
		})
	}
}
