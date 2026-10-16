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

package resolver

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fluxcd/pkg/apis/meta"
	sourcev1 "github.com/fluxcd/source-controller/api/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/deckhouse/chart-values-controller/internal/cache"
	"github.com/deckhouse/chart-values-controller/internal/chartartifact"
	"github.com/deckhouse/chart-values-controller/internal/labels"
	cvnaming "github.com/deckhouse/chart-values-controller/internal/naming"
	"github.com/deckhouse/operator-helm/api/naming"
	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
)

const testCacheBudget = int64(16 << 20)

func newTestResolver(t *testing.T, objects ...client.Object) *Resolver {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("registering client-go scheme: %v", err)
	}
	if err := helmv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("registering helm scheme: %v", err)
	}
	if err := sourcev1.AddToScheme(scheme); err != nil {
		t.Fatalf("registering source scheme: %v", err)
	}

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objects...).
		WithStatusSubresource(
			&helmv1alpha1.HelmClusterAddonRepository{},
			&helmv1alpha1.HelmApplicationRepository{},
			&helmv1alpha1.HelmClusterApplicationRepository{},
		).
		Build()

	return &Resolver{
		client:         c,
		cache:          cache.New(testCacheBudget),
		namespace:      "d8-operator-helm",
		ttl:            time.Hour,
		sourceInterval: metav1.Duration{Duration: time.Minute},
	}
}

func readySource(name, digest string, expiresAt time.Time) *sourcev1.HelmChart {
	return &sourcev1.HelmChart{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   "d8-operator-helm",
			Labels:      map[string]string{labels.ManagedBy: labels.ManagedByValue},
			Annotations: map[string]string{labels.ExpiresAtAnnotation: expiresAt.UTC().Format(time.RFC3339)},
		},
		Status: sourcev1.HelmChartStatus{
			Conditions: []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue}},
			Artifact:   &meta.Artifact{Digest: digest},
		},
	}
}

func chartWithVersions(repoName, chartName string, versions ...helmv1alpha1.ChartVersion) *helmv1alpha1.HelmClusterAddonChart {
	return &helmv1alpha1.HelmClusterAddonChart{
		ObjectMeta: metav1.ObjectMeta{Name: naming.HelmClusterAddonChartName(repoName, chartName)},
		Status:     helmv1alpha1.ChartCatalogStatus{Versions: versions},
	}
}

func TestOCIMediaType(t *testing.T) {
	req := Request{Kind: RepositoryKindHelmClusterAddon, RepositoryName: "example", Chart: "podinfo", Version: "6.7.1"}

	t.Run("a usable version returns its media type", func(t *testing.T) {
		mediaType, done := ociMediaType(req, &helmv1alpha1.ChartVersion{
			Version: "6.7.1", MediaType: "application/tar+gzip",
		})
		if done != nil {
			t.Fatalf("expected to continue, got outcome %q", done.Outcome)
		}
		if mediaType != "application/tar+gzip" {
			t.Fatalf("media type is %q", mediaType)
		}
	})

	t.Run("an unusable version is values_not_found with the reason", func(t *testing.T) {
		_, done := ociMediaType(req, &helmv1alpha1.ChartVersion{
			Version:            "6.7.1",
			UnavailableReason:  helmv1alpha1.UnavailableReasonUnsupportedMediaType,
			UnavailableMessage: "config media type \"application/vnd.unknown.config.v1+json\" is not a helm chart config",
		})
		if done == nil || done.Outcome != OutcomeValuesNotFound {
			t.Fatalf("outcome is %+v, want values_not_found", done)
		}
		if !strings.Contains(done.Message, helmv1alpha1.UnavailableReasonUnsupportedMediaType) {
			t.Fatalf("message %q must name the reason", done.Message)
		}
	})

	t.Run("a resolve-pending version is pending, not values_not_found", func(t *testing.T) {
		_, done := ociMediaType(req, &helmv1alpha1.ChartVersion{
			Version:           "6.7.1",
			UnavailableReason: helmv1alpha1.UnavailableReasonResolvePending,
		})
		if done == nil || done.Outcome != OutcomePending {
			t.Fatalf("outcome is %+v, want pending: a resolve-pending verdict is self-healing and must be retried, not reported as a permanent failure", done)
		}
	})

	t.Run("a pre-upgrade version with no verdict at all is pending, not values_not_found", func(t *testing.T) {
		_, done := ociMediaType(req, &helmv1alpha1.ChartVersion{Version: "6.7.1"})
		if done == nil || done.Outcome != OutcomePending {
			t.Fatalf("outcome is %+v, want pending: an empty verdict is the pre-upgrade migration state and must be retried, not reported as a permanent failure", done)
		}
	})

	t.Run("a removed version keeps its media type usable", func(t *testing.T) {
		mediaType, done := ociMediaType(req, &helmv1alpha1.ChartVersion{
			Version:           "6.7.1",
			MediaType:         "application/tar+gzip",
			UnavailableReason: helmv1alpha1.UnavailableReasonRemovedFromRepository,
		})
		if done != nil {
			t.Fatalf("expected to continue, got outcome %q", done.Outcome)
		}
		if mediaType != "application/tar+gzip" {
			t.Fatalf("media type is %q", mediaType)
		}
	})
}

func TestChartVersion(t *testing.T) {
	req := Request{Kind: RepositoryKindHelmClusterAddon, RepositoryName: "example", Chart: "podinfo", Version: "6.7.1"}

	t.Run("an existing version is returned as recorded", func(t *testing.T) {
		resolver := newTestResolver(t, chartWithVersions("example", "podinfo",
			helmv1alpha1.ChartVersion{Version: "6.7.1", MediaType: "application/tar+gzip"},
		))

		version, done, err := resolver.chartVersion(context.Background(), mustFamily(t, req.Kind), req)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if done != nil {
			t.Fatalf("expected to continue, got outcome %q", done.Outcome)
		}
		if version.MediaType != "application/tar+gzip" {
			t.Fatalf("media type is %q", version.MediaType)
		}
	})

	t.Run("an archive version with nothing recorded is still returned", func(t *testing.T) {
		resolver := newTestResolver(t, chartWithVersions("example", "podinfo",
			helmv1alpha1.ChartVersion{Version: "6.7.1"},
		))

		version, done, err := resolver.chartVersion(context.Background(), mustFamily(t, req.Kind), req)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if done != nil {
			t.Fatalf("outcome is %q, want the entry to be returned", done.Outcome)
		}
		if version.Version != "6.7.1" {
			t.Fatalf("version is %q", version.Version)
		}
	})

	t.Run("an unaddressable index reference is values_not_found", func(t *testing.T) {
		resolver := newTestResolver(t, chartWithVersions("example", "podinfo",
			helmv1alpha1.ChartVersion{
				Version:            "6.7.1",
				UnavailableReason:  helmv1alpha1.UnavailableReasonInvalidChartReference,
				UnavailableMessage: "oci reference \"oci://BAD_HOST//:::\" is not a valid tagged reference",
			},
		))

		_, done, err := resolver.chartVersion(context.Background(), mustFamily(t, req.Kind), req)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if done == nil || done.Outcome != OutcomeValuesNotFound {
			t.Fatalf("outcome is %+v, want values_not_found", done)
		}
	})

	t.Run("a missing chart is pending", func(t *testing.T) {
		resolver := newTestResolver(t)

		_, done, err := resolver.chartVersion(context.Background(), mustFamily(t, req.Kind), req)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if done == nil || done.Outcome != OutcomePending {
			t.Fatalf("outcome is %+v, want pending", done)
		}
	})

	t.Run("a missing version is pending", func(t *testing.T) {
		resolver := newTestResolver(t, chartWithVersions("example", "podinfo",
			helmv1alpha1.ChartVersion{Version: "6.7.0", MediaType: "application/tar+gzip"},
		))

		_, done, err := resolver.chartVersion(context.Background(), mustFamily(t, req.Kind), req)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if done == nil || done.Outcome != OutcomePending {
			t.Fatalf("outcome is %+v, want pending", done)
		}
	})
}

type stubProber struct {
	mediaType string
	err       error
	calls     int
	refs      []string
}

func (p *stubProber) ChartLayerMediaType(_ context.Context, ref string, _ http.RoundTripper) (string, error) {
	p.calls++
	p.refs = append(p.refs, ref)

	return p.mediaType, p.err
}

func newHybridResolver(t *testing.T, prober chartartifact.Prober, objects ...client.Object) (*Resolver, client.Client) {
	t.Helper()

	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		clientgoscheme.AddToScheme,
		helmv1alpha1.AddToScheme,
		sourcev1.AddToScheme,
	} {
		if err := add(scheme); err != nil {
			t.Fatalf("registering scheme: %v", err)
		}
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()

	return &Resolver{
		client:           c,
		cache:            cache.New(testCacheBudget),
		namespace:        "d8-operator-helm",
		ttl:              time.Minute,
		sourceInterval:   metav1.Duration{Duration: time.Minute},
		maxArtifactBytes: 1 << 20,
		prober:           prober,
	}, c
}

func TestResolveHybridVersionUsesOCIRepository(t *testing.T) {
	repo := &helmv1alpha1.HelmClusterAddonRepository{
		ObjectMeta: metav1.ObjectMeta{Name: "example", UID: "repo-uid", Generation: 3},
		Spec:       helmv1alpha1.RepositorySpec{URL: "https://charts.example.invalid/stable"},
	}
	chart := chartWithVersions("example", "nginx", helmv1alpha1.ChartVersion{
		Version: "0.1.0",
		OCIRef:  "oci://ghcr.io/drey/nginx/nginx:0.1.0",
	})

	req := Request{
		Kind:           RepositoryKindHelmClusterAddon,
		RepositoryName: "example",
		Chart:          "nginx",
		Version:        "0.1.0",
	}

	prober := &stubProber{mediaType: "application/vnd.cncf.helm.chart.content.v1.tar+gzip"}

	resolver, c := newHybridResolver(t, prober, repo, chart)

	if _, err := resolver.resolveChart(context.Background(), mustFamily(t, req.Kind), req); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	name := cvnaming.AuxResourceName(string(req.Kind), req.Namespace, req.RepositoryName,
		req.Chart, req.Version, identityOf(repo.ObjectMeta))
	key := client.ObjectKey{Name: name, Namespace: "d8-operator-helm"}

	ociRepo := &sourcev1.OCIRepository{}
	if err := c.Get(context.Background(), key, ociRepo); err != nil {
		t.Fatalf("a version published in a registry must be read through an oci repository: %v", err)
	}
	if ociRepo.Spec.URL != "oci://ghcr.io/drey/nginx/nginx" {
		t.Fatalf("url = %q, want the address from the index reference", ociRepo.Spec.URL)
	}
	if ociRepo.Spec.Reference == nil || ociRepo.Spec.Reference.Tag != "0.1.0" {
		t.Fatalf("reference = %+v, want tag 0.1.0", ociRepo.Spec.Reference)
	}

	if ociRepo.Spec.LayerSelector == nil || ociRepo.Spec.LayerSelector.MediaType != prober.mediaType {
		t.Fatalf("layer selector = %+v, want the probed media type", ociRepo.Spec.LayerSelector)
	}

	if prober.calls != 1 {
		t.Fatalf("prober calls = %d, want 1", prober.calls)
	}
	if prober.refs[0] != "oci://ghcr.io/drey/nginx/nginx:0.1.0" {
		t.Fatalf("probed %q, want the recorded reference", prober.refs[0])
	}

	if ociRepo.Spec.SecretRef != nil || ociRepo.Spec.CertSecretRef != nil {
		t.Error("the repository's secrets must not be attached to a foreign registry")
	}

	helmChart := &sourcev1.HelmChart{}
	if err := c.Get(context.Background(), key, helmChart); err == nil {
		t.Fatal("no helm chart must be created for a version published in a registry")
	}
}

func TestResolveArchiveVersionUsesHelmChart(t *testing.T) {
	repo := &helmv1alpha1.HelmClusterAddonRepository{
		ObjectMeta: metav1.ObjectMeta{Name: "bitnami"},
		Spec:       helmv1alpha1.RepositorySpec{URL: "https://charts.example.invalid/bitnami"},
	}
	helmRepo := &sourcev1.HelmRepository{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "hcar-bitnami",
			Namespace: "d8-operator-helm",
			Labels:    map[string]string{helmv1alpha1.HelmClusterAddonRepositoryLabelSourceName: "bitnami"},
		},
	}
	chart := chartWithVersions("bitnami", "nginx", helmv1alpha1.ChartVersion{Version: "0.2.0"})

	req := Request{
		Kind:           RepositoryKindHelmClusterAddon,
		RepositoryName: "bitnami",
		Chart:          "nginx",
		Version:        "0.2.0",
	}

	resolver, c := newHybridResolver(t, nil, repo, chart, helmRepo)

	result, err := resolver.resolveChart(context.Background(), mustFamily(t, req.Kind), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Outcome == OutcomeValuesNotFound {
		t.Fatalf("an archive version must not be reported as unreadable: %+v", result)
	}

	name := cvnaming.AuxResourceName(string(req.Kind), req.Namespace, req.RepositoryName,
		req.Chart, req.Version, identityOf(repo.ObjectMeta))
	key := client.ObjectKey{Name: name, Namespace: "d8-operator-helm"}

	helmChart := &sourcev1.HelmChart{}
	if err := c.Get(context.Background(), key, helmChart); err != nil {
		t.Fatalf("an archive version must be read through a helm chart: %v", err)
	}
	if helmChart.Spec.Version != "0.2.0" {
		t.Fatalf("chart version = %q, want 0.2.0", helmChart.Spec.Version)
	}
}

func TestResolveDispatchesOnTheRequestShape(t *testing.T) {
	r := newTestResolver(t)

	cases := []struct {
		name string
		req  Request
		want Outcome
	}{
		{
			name: "unknown kind",
			req:  Request{Kind: "somethingelse", RepositoryName: "example", Chart: "podinfo", Version: "6.7.1"},
			want: OutcomeUnsupportedRepositoryKind,
		},
		{
			name: "namespaced kind without a namespace",
			req:  Request{Kind: RepositoryKindHelmApplication, RepositoryName: "stable", Chart: "podinfo", Version: "6.7.1"},
			want: OutcomeInvalidRequest,
		},
		{
			name: "cluster addon kind with a namespace is not rejected",
			req:  Request{Kind: RepositoryKindHelmClusterAddon, Namespace: "team-a", RepositoryName: "example", Chart: "podinfo", Version: "6.7.1"},
			want: OutcomeRepositoryNotFound,
		},
		{
			name: "cluster application kind with a namespace is not rejected",
			req:  Request{Kind: RepositoryKindHelmClusterApplication, Namespace: "team-a", RepositoryName: "shared", Chart: "podinfo", Version: "6.7.1"},
			want: OutcomeRepositoryNotFound,
		},
		{
			name: "kind casing does not matter",
			req:  Request{Kind: "HelmApplicationRepository", Namespace: "team-a", RepositoryName: "missing", Chart: "podinfo", Version: "6.7.1"},
			want: OutcomeRepositoryNotFound,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := r.Resolve(context.Background(), tc.req)
			if err != nil {
				t.Fatalf("Resolve returned %v", err)
			}
			if got.Outcome != tc.want {
				t.Fatalf("outcome = %q, want %q (message %q)", got.Outcome, tc.want, got.Message)
			}
		})
	}
}

func TestClusterScopedRequestsIgnoreAStrayNamespace(t *testing.T) {
	for _, kind := range []RepositoryKind{RepositoryKindHelmClusterAddon, RepositoryKindHelmClusterApplication} {
		t.Run(string(kind), func(t *testing.T) {
			meta := metav1.ObjectMeta{Name: "example", UID: "repo-uid", Generation: 2}
			spec := helmv1alpha1.RepositorySpec{URL: "https://charts.example.invalid/stable"}

			var repo client.Object = &helmv1alpha1.HelmClusterAddonRepository{ObjectMeta: meta, Spec: spec}
			var chart client.Object = chartWithVersions("example", "podinfo", helmv1alpha1.ChartVersion{Version: "6.7.1"})
			if kind == RepositoryKindHelmClusterApplication {
				repo = &helmv1alpha1.HelmClusterApplicationRepository{ObjectMeta: meta, Spec: spec}
				chart = &helmv1alpha1.HelmClusterApplicationChart{
					ObjectMeta: metav1.ObjectMeta{Name: naming.HelmClusterApplicationChartName("example", "podinfo")},
					Status:     helmv1alpha1.ChartCatalogStatus{Versions: []helmv1alpha1.ChartVersion{{Version: "6.7.1"}}},
				}
			}

			req := Request{Kind: kind, RepositoryName: "example", Chart: "podinfo", Version: "6.7.1"}
			name := cvnaming.AuxResourceName(string(kind), "",
				req.RepositoryName, req.Chart, req.Version, identityOf(meta))

			r := newTestResolver(t, repo, chart, readySource(name, "sha256:seeded", time.Now().Add(time.Hour)))
			if err := r.cache.Put(name, "sha256:seeded", []byte("replicaCount: 1\n")); err != nil {
				t.Fatalf("seeding cache: %v", err)
			}

			withNamespace := req
			withNamespace.Namespace = "team-a"

			got, err := r.Resolve(context.Background(), withNamespace)
			if err != nil {
				t.Fatalf("Resolve returned %v", err)
			}
			if got.Outcome != OutcomeReady || string(got.Values) != "replicaCount: 1\n" {
				t.Fatalf("got %+v, want the entry cached without a namespace: a stray namespace must not miss it or create a second identity", got)
			}
		})
	}
}

func TestReadValuesCachesNothingFromARefusedArchive(t *testing.T) {
	archive := buildBombArchive(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(archive)
	}))
	t.Cleanup(srv.Close)

	r := &Resolver{
		cache:            cache.New(testCacheBudget),
		httpClient:       srv.Client(),
		maxArtifactBytes: 100 << 20,
	}

	digest := fmt.Sprintf("sha256:%x", sha256.Sum256(archive))

	got, err := r.readValues(context.Background(), "entry", &meta.Artifact{URL: srv.URL, Digest: digest})
	if err != nil {
		t.Fatalf("readValues returned %v", err)
	}
	if got.Outcome != OutcomeFetchFailed {
		t.Fatalf("outcome = %q, want fetch_failed (message %q)", got.Outcome, got.Message)
	}

	if _, ok := r.cache.Get("entry", digest); ok {
		t.Fatal("the cache holds an entry, want none: a refused archive must not be served to the next request")
	}
}

func buildBombArchive(t *testing.T) []byte {
	t.Helper()

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	files := []struct {
		name    string
		content []byte
	}{
		{"podinfo/big.bin", make([]byte, 64<<20)},
		{"podinfo/values.yaml", []byte("replicaCount: 1\n")},
	}

	for _, file := range files {
		if err := tw.WriteHeader(&tar.Header{
			Name:     file.name,
			Mode:     0o644,
			Size:     int64(len(file.content)),
			Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatalf("writing tar header: %v", err)
		}
		if _, err := tw.Write(file.content); err != nil {
			t.Fatalf("writing tar content: %v", err)
		}
	}

	if err := tw.Close(); err != nil {
		t.Fatalf("closing tar: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("closing gzip: %v", err)
	}

	return buf.Bytes()
}

func mustFamily(t *testing.T, kind RepositoryKind) repositoryFamily {
	t.Helper()

	family, ok := familyFor(kind)
	if !ok {
		t.Fatalf("no family for kind %q", kind)
	}

	return family
}

func TestAReplacedRepositoryIsNotServedFromTheOldEntry(t *testing.T) {
	const (
		repoName = "example"
		chart    = "podinfo"
		version  = "6.7.1"
	)

	previous := metav1.ObjectMeta{Name: repoName, UID: "repo-uid", Generation: 1}

	tests := []struct {
		name string
		now  metav1.ObjectMeta
	}{
		{
			name: "recreated under the same name",
			now:  metav1.ObjectMeta{Name: repoName, UID: "another-uid", Generation: 1},
		},
		{
			name: "edited to point somewhere else",
			now:  metav1.ObjectMeta{Name: repoName, UID: "repo-uid", Generation: 2},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newTestResolver(t, &helmv1alpha1.HelmClusterAddonRepository{ObjectMeta: tt.now})

			stale := cvnaming.AuxResourceName(string(RepositoryKindHelmClusterAddon), "",
				repoName, chart, version, identityOf(previous))
			if err := r.cache.Put(stale, "sha256:seeded", []byte("replicaCount: 1\n")); err != nil {
				t.Fatalf("seeding cache: %v", err)
			}

			got, err := r.Resolve(context.Background(), Request{
				Kind:           RepositoryKindHelmClusterAddon,
				RepositoryName: repoName,
				Chart:          chart,
				Version:        version,
			})
			if err != nil {
				t.Fatalf("Resolve returned %v", err)
			}
			if got.Outcome == OutcomeReady {
				t.Fatalf("values were served from the previous repository: %+v", got)
			}
		})
	}
}

func TestAStatusWriteDoesNotInvalidateTheEntry(t *testing.T) {
	const (
		repoName = "example"
		chart    = "podinfo"
		version  = "6.7.1"
	)

	repo := &helmv1alpha1.HelmClusterAddonRepository{
		ObjectMeta: metav1.ObjectMeta{Name: repoName, UID: "repo-uid", Generation: 1},
		Spec:       helmv1alpha1.RepositorySpec{URL: "https://charts.example.invalid/stable"},
	}

	name := cvnaming.AuxResourceName(string(RepositoryKindHelmClusterAddon), "",
		repoName, chart, version, identityOf(repo.ObjectMeta))

	r := newTestResolver(t, repo, chartWithVersions(repoName, chart, helmv1alpha1.ChartVersion{Version: version}),
		readySource(name, "sha256:seeded", time.Now().Add(time.Hour)))
	if err := r.cache.Put(name, "sha256:seeded", []byte("replicaCount: 1\n")); err != nil {
		t.Fatalf("seeding cache: %v", err)
	}

	stored := &helmv1alpha1.HelmClusterAddonRepository{}
	if err := r.client.Get(context.Background(), client.ObjectKey{Name: repoName}, stored); err != nil {
		t.Fatalf("reading repository: %v", err)
	}
	stored.Status.ObservedGeneration = 1
	stored.Status.Conditions = []metav1.Condition{{
		Type:               helmv1alpha1.ConditionTypeReady,
		Status:             metav1.ConditionTrue,
		Reason:             helmv1alpha1.ReasonSuccess,
		LastTransitionTime: metav1.Now(),
	}}
	if err := r.client.Status().Update(context.Background(), stored); err != nil {
		t.Fatalf("updating repository status: %v", err)
	}

	got, err := r.Resolve(context.Background(), Request{
		Kind:           RepositoryKindHelmClusterAddon,
		RepositoryName: repoName,
		Chart:          chart,
		Version:        version,
	})
	if err != nil {
		t.Fatalf("Resolve returned %v", err)
	}
	if got.Outcome != OutcomeReady || string(got.Values) != "replicaCount: 1\n" {
		t.Fatalf("got %+v, want the cached entry: a status write must not cost a re-download", got)
	}
}

func TestAHitIsServedOnlyAtTheRevisionTheObjectHolds(t *testing.T) {
	repo := &helmv1alpha1.HelmClusterAddonRepository{ObjectMeta: metav1.ObjectMeta{Name: "example", UID: "repo-uid", Generation: 1},
		Spec: helmv1alpha1.RepositorySpec{URL: "https://charts.example.invalid/stable"}}
	req := Request{Kind: RepositoryKindHelmClusterAddon, RepositoryName: "example", Chart: "podinfo", Version: "6.7.1"}
	name := cvnaming.AuxResourceName(string(req.Kind), "", req.RepositoryName, req.Chart, req.Version, identityOf(repo.ObjectMeta))

	tests := []struct {
		name   string
		cached string
		want   Outcome
	}{
		{name: "the cache holds the object's revision", cached: "sha256:current", want: OutcomeReady},
		{name: "the cache holds another revision", cached: "sha256:stale", want: OutcomePending},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newTestResolver(t, repo, chartWithVersions("example", "podinfo", helmv1alpha1.ChartVersion{Version: "6.7.1"}),
				readySource(name, "sha256:current", time.Now().Add(time.Hour)))
			if err := r.cache.Put(name, tt.cached, []byte("replicaCount: 1\n")); err != nil {
				t.Fatalf("seeding cache: %v", err)
			}

			got, err := r.Resolve(context.Background(), req)
			if err != nil {
				t.Fatalf("Resolve returned %v", err)
			}
			if got.Outcome != tt.want {
				t.Fatalf("outcome = %q (%s), want %q", got.Outcome, got.Message, tt.want)
			}
		})
	}
}

func TestAHitRenewsAnExpiryThatIsMoreThanHalfSpent(t *testing.T) {
	repo := &helmv1alpha1.HelmClusterAddonRepository{ObjectMeta: metav1.ObjectMeta{Name: "example", UID: "repo-uid", Generation: 1},
		Spec: helmv1alpha1.RepositorySpec{URL: "https://charts.example.invalid/stable"}}
	helmRepo := &sourcev1.HelmRepository{ObjectMeta: metav1.ObjectMeta{
		Name:      "hcar-example",
		Namespace: "d8-operator-helm",
		Labels:    map[string]string{helmv1alpha1.HelmClusterAddonRepositoryLabelSourceName: "example"},
	}}
	req := Request{Kind: RepositoryKindHelmClusterAddon, RepositoryName: "example", Chart: "podinfo", Version: "6.7.1"}
	name := cvnaming.AuxResourceName(string(req.Kind), "", req.RepositoryName, req.Chart, req.Version, identityOf(repo.ObjectMeta))

	tests := []struct {
		name    string
		left    time.Duration
		renewed bool
	}{
		{name: "less than half the ttl left", left: 15 * time.Minute, renewed: true},
		{name: "more than half the ttl left", left: 45 * time.Minute, renewed: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			source := readySource(name, "sha256:current", time.Now().Add(tt.left))
			original := source.Annotations[labels.ExpiresAtAnnotation]

			r := newTestResolver(t, repo, helmRepo, chartWithVersions("example", "podinfo", helmv1alpha1.ChartVersion{Version: "6.7.1"}), source)
			if err := r.cache.Put(name, "sha256:current", []byte("replicaCount: 1\n")); err != nil {
				t.Fatalf("seeding cache: %v", err)
			}

			got, err := r.Resolve(context.Background(), req)
			if err != nil {
				t.Fatalf("Resolve returned %v", err)
			}
			if got.Outcome != OutcomeReady || string(got.Values) != "replicaCount: 1\n" {
				t.Fatalf("got %+v, want the cached values", got)
			}

			stored := &sourcev1.HelmChart{}
			if err := r.client.Get(context.Background(), client.ObjectKey{Namespace: "d8-operator-helm", Name: name}, stored); err != nil {
				t.Fatalf("reading helm chart: %v", err)
			}
			annotation := stored.Annotations[labels.ExpiresAtAnnotation]

			if !tt.renewed {
				if annotation != original {
					t.Fatalf("expires-at = %q, want it left at %q", annotation, original)
				}
				return
			}

			if annotation == original {
				t.Fatalf("expires-at stayed at %q, want it renewed", original)
			}
			expiresAt, err := time.Parse(time.RFC3339, annotation)
			if err != nil {
				t.Fatalf("parsing expires-at %q: %v", annotation, err)
			}
			if left := time.Until(expiresAt); left < 59*time.Minute {
				t.Fatalf("expires-at is %s away, want a full ttl", left)
			}
		})
	}
}

func TestATransportTimeoutIsAnsweredAsPendingWhoeverHitIt(t *testing.T) {
	hang := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		<-hang
	}))
	t.Cleanup(func() {
		close(hang)
		srv.Close()
	})

	c := srv.Client()
	c.Timeout = 50 * time.Millisecond

	r := &Resolver{cache: cache.New(testCacheBudget), httpClient: c, maxArtifactBytes: 1 << 20}

	result, err := r.readValues(context.Background(), "entry", &meta.Artifact{URL: srv.URL})
	if err != nil {
		t.Fatalf("readValues returned an error: %v", err)
	}
	if result.Outcome != OutcomePending {
		t.Fatalf("outcome = %q (%s), want %q", result.Outcome, result.Message, OutcomePending)
	}
}

func TestConcurrentMissesFetchTheArtifactOnce(t *testing.T) {
	archive := buildValuesArchive(t, "replicaCount: 1\n")

	tests := []struct {
		name        string
		key         func(caller int) string
		wantFetches int64
	}{
		{
			name:        "the same chart of the same repository is downloaded once",
			key:         func(int) string { return "entry" },
			wantFetches: 1,
		},
		{
			name:        "charts under different keys are downloaded independently",
			key:         func(caller int) string { return fmt.Sprintf("entry-%d", caller) },
			wantFetches: 4,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			const callers = 4

			var fetches atomic.Int64
			release := make(chan struct{})
			downloading := make(chan struct{}, callers)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				fetches.Add(1)
				downloading <- struct{}{}
				<-release
				_, _ = w.Write(archive)
			}))
			t.Cleanup(srv.Close)

			art := &meta.Artifact{URL: srv.URL, Digest: fmt.Sprintf("sha256:%x", sha256.Sum256(archive))}

			r := &Resolver{
				cache:            cache.New(testCacheBudget),
				httpClient:       srv.Client(),
				maxArtifactBytes: 1 << 20,
			}

			var joined sync.WaitGroup
			joined.Add(callers)

			results := make([]Result, callers)

			var wg sync.WaitGroup
			for i := range callers {
				var once sync.Once
				ctx := joinSignal{Context: context.Background(), joined: func() { once.Do(joined.Done) }}

				wg.Add(1)
				go func() {
					defer wg.Done()

					got, err := r.readValues(ctx, tt.key(i), art)
					if err != nil {
						t.Errorf("caller %d: readValues returned %v", i, err)
						return
					}
					results[i] = got
				}()
			}

			for range tt.wantFetches {
				<-downloading
			}
			joined.Wait()
			close(release)
			wg.Wait()

			if got := fetches.Load(); got != tt.wantFetches {
				t.Fatalf("the artifact was downloaded %d times, want %d", got, tt.wantFetches)
			}

			for i, got := range results {
				if got.Outcome != OutcomeReady || string(got.Values) != "replicaCount: 1\n" {
					t.Fatalf("caller %d got %+v, want the chart's values", i, got)
				}
			}
		})
	}
}

func TestConcurrentMissesAtTwoRevisionsOfOneNameDoNotShareADownload(t *testing.T) {
	release := make(chan struct{})
	serve := func(archive []byte) *httptest.Server {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			<-release
			_, _ = w.Write(archive)
		}))
		t.Cleanup(srv.Close)
		return srv
	}
	archiveA, archiveB := buildValuesArchive(t, "replicaCount: 1\n"), buildValuesArchive(t, "replicaCount: 2\n")
	artA := &meta.Artifact{URL: serve(archiveA).URL, Digest: fmt.Sprintf("sha256:%x", sha256.Sum256(archiveA))}
	artB := &meta.Artifact{URL: serve(archiveB).URL, Digest: fmt.Sprintf("sha256:%x", sha256.Sum256(archiveB))}

	r := &Resolver{cache: cache.New(testCacheBudget), httpClient: http.DefaultClient, maxArtifactBytes: 1 << 20}

	var joined sync.WaitGroup
	var wg sync.WaitGroup
	results := make([]Result, 2)
	for i, art := range []*meta.Artifact{artA, artB} {
		var once sync.Once
		joined.Add(1)
		ctx := joinSignal{Context: context.Background(), joined: func() { once.Do(joined.Done) }}

		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := r.readValues(ctx, "entry", art)
			if err != nil {
				t.Errorf("revision %d: %v", i, err)
				return
			}
			results[i] = got
		}()
	}
	joined.Wait()
	close(release)
	wg.Wait()

	if string(results[0].Values) != "replicaCount: 1\n" || string(results[1].Values) != "replicaCount: 2\n" {
		t.Fatalf("got %q and %q: a request that saw a newer revision must not receive the values of the older one",
			results[0].Values, results[1].Values)
	}
}

type panickingTransport struct{}

func (panickingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	panic("transport exploded")
}

func TestAPanicInsideTheSharedDownloadIsAnsweredAsAFailureNotACrash(t *testing.T) {
	r := &Resolver{
		cache:            cache.New(testCacheBudget),
		httpClient:       &http.Client{Transport: panickingTransport{}},
		maxArtifactBytes: 1 << 20,
	}

	got, err := r.readValues(context.Background(), "entry",
		&meta.Artifact{URL: "http://artifact.invalid/chart.tgz", Digest: "sha256:any"})
	if err != nil {
		t.Fatalf("readValues returned %v", err)
	}
	if got.Outcome != OutcomeFetchFailed || !strings.Contains(got.Message, "transport exploded") {
		t.Fatalf("got %+v, want fetch_failed naming the panic", got)
	}
}

func TestAWaiterGetsTheValuesWhenTheLeaderGivesUp(t *testing.T) {
	archive := buildValuesArchive(t, "replicaCount: 1\n")

	started := make(chan struct{})
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
		_, _ = w.Write(archive)
	}))
	t.Cleanup(srv.Close)
	var releaseOnce sync.Once
	releaseServer := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseServer)

	art := &meta.Artifact{URL: srv.URL, Digest: fmt.Sprintf("sha256:%x", sha256.Sum256(archive))}
	r := &Resolver{
		cache:            cache.New(testCacheBudget),
		httpClient:       srv.Client(),
		maxArtifactBytes: 1 << 20,
	}

	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	t.Cleanup(cancelLeader)

	leader := make(chan Result, 1)
	go func() {
		got, err := r.readValues(leaderCtx, "entry", art)
		if err != nil {
			t.Errorf("the leader: readValues returned %v", err)
		}
		leader <- got
	}()
	<-started

	joined := make(chan struct{})
	var joinOnce sync.Once
	waiterCtx := joinSignal{Context: context.Background(), joined: func() { joinOnce.Do(func() { close(joined) }) }}

	waiter := make(chan Result, 1)
	go func() {
		got, err := r.readValues(waiterCtx, "entry", art)
		if err != nil {
			t.Errorf("the waiter: readValues returned %v", err)
		}
		waiter <- got
	}()
	<-joined

	cancelLeader()
	select {
	case got := <-leader:
		if got.Outcome != OutcomePending {
			t.Fatalf("the leader got %+v after its context was cancelled, want %q", got, OutcomePending)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the leader did not return after its context was cancelled: it must not outlast its own budget waiting on the shared download")
	}
	releaseServer()

	if got := <-waiter; got.Outcome != OutcomeReady || string(got.Values) != "replicaCount: 1\n" {
		t.Fatalf("the waiter got %+v, want the chart's values: the leader's cancellation must not fail the shared download", got)
	}

	if _, ok := r.cache.Get("entry", art.Digest); !ok {
		t.Fatal("the downloaded revision is not cached, want it cached despite the leader giving up")
	}
}

// joinSignal reports that a caller of readValues is waiting on the shared download: readValues uses the context for nothing else.
type joinSignal struct {
	context.Context
	joined func()
}

func (c joinSignal) Done() <-chan struct{} {
	c.joined()

	return c.Context.Done()
}

func buildValuesArchive(t *testing.T, values string) []byte {
	t.Helper()

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	if err := tw.WriteHeader(&tar.Header{
		Name:     "podinfo/values.yaml",
		Mode:     0o644,
		Size:     int64(len(values)),
		Typeflag: tar.TypeReg,
	}); err != nil {
		t.Fatalf("writing tar header: %v", err)
	}
	if _, err := tw.Write([]byte(values)); err != nil {
		t.Fatalf("writing tar content: %v", err)
	}

	if err := tw.Close(); err != nil {
		t.Fatalf("closing tar: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("closing gzip: %v", err)
	}

	return buf.Bytes()
}

func TestAFullCacheStillAnswersTheRequest(t *testing.T) {
	archive := buildValuesArchive(t, "replicaCount: 1\n")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(archive)
	}))
	t.Cleanup(srv.Close)

	r := &Resolver{
		cache:            cache.New(1),
		httpClient:       srv.Client(),
		maxArtifactBytes: 1 << 20,
	}

	digest := fmt.Sprintf("sha256:%x", sha256.Sum256(archive))

	got, err := r.readValues(context.Background(), "entry", &meta.Artifact{URL: srv.URL, Digest: digest})
	if err != nil {
		t.Fatalf("readValues returned %v", err)
	}
	if got.Outcome != OutcomeReady || string(got.Values) != "replicaCount: 1\n" {
		t.Fatalf("got %+v, want the values served from the download despite the full cache", got)
	}

	if _, ok := r.cache.Get("entry", digest); ok {
		t.Fatal("the cache holds an entry, want none: nothing may be stored past the budget")
	}
}
