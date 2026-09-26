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
	"os"
	"strings"
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
	cvnaming "github.com/deckhouse/chart-values-controller/internal/naming"
	"github.com/deckhouse/operator-helm/api/naming"
	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
)

func newTestResolver(t *testing.T, objects ...client.Object) *Resolver {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("registering client-go scheme: %v", err)
	}
	if err := helmv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("registering helm scheme: %v", err)
	}

	// The repository kinds carry a status subresource in the cluster, and a test
	// that writes one has to go through the same door the operator does — on a fake
	// client without this, a status write is an ordinary update and moves the
	// generation, which is exactly what such a test is checking does not happen.
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objects...).
		WithStatusSubresource(
			&helmv1alpha1.HelmClusterAddonRepository{},
			&helmv1alpha1.HelmApplicationRepository{},
			&helmv1alpha1.HelmClusterApplicationRepository{},
		).
		Build()

	return &Resolver{client: c, cache: cache.New(t.TempDir())}
}

func chartWithVersions(repoName, chartName string, versions ...helmv1alpha1.ChartVersion) *helmv1alpha1.HelmClusterAddonChart {
	return &helmv1alpha1.HelmClusterAddonChart{
		ObjectMeta: metav1.ObjectMeta{Name: naming.HelmClusterAddonChartName(repoName, chartName)},
		Status:     helmv1alpha1.ChartCatalogStatus{Versions: versions},
	}
}

// TestOCIMediaType covers the verdict an oci:// repository's version carries. The
// media type is that repository's only locator, so its absence is a state rather than
// a value — which is exactly why this judgement must not be applied to a version of a
// helm repository, where an absent media type is normal.
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
		// Neither MediaType nor UnavailableReason set: the shape a version entry of an
		// oci:// repository had before this controller started recording verdicts. The
		// migration path (client.KnownVersions) treats this exactly like ResolvePending
		// and re-resolves it on the next normal synchronization.
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
		// The regression this guards: an entry with no media type, no reference and no
		// verdict is a perfectly normal archive version, and reading it as "unresolved"
		// made every such version report pending forever.
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
		cache:            cache.New(t.TempDir()),
		namespace:        "d8-operator-helm",
		ttl:              time.Minute,
		sourceInterval:   metav1.Duration{Duration: time.Minute},
		maxArtifactBytes: 1 << 20,
		prober:           prober,
	}, c
}

// TestResolveHybridVersionUsesOCIRepository pins the whole point of a hybrid
// repository for this controller: a classic HTTP repository whose index publishes one
// version in a registry must have that version read through an OCIRepository. Sending
// it down the HelmChart path makes the source controller resolve the version to the
// index url — an oci:// one — and fail with `unsupported protocol scheme "oci"`.
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

	// The catalog does not record a media type for such a version, so the probe is the
	// only way this service can learn the layer.
	if prober.calls != 1 {
		t.Fatalf("prober calls = %d, want 1", prober.calls)
	}
	if prober.refs[0] != "oci://ghcr.io/drey/nginx/nginx:0.1.0" {
		t.Fatalf("probed %q, want the recorded reference", prober.refs[0])
	}

	// Only public registries are supported for such a version: the repository's own
	// credentials describe a different host.
	if ociRepo.Spec.SecretRef != nil || ociRepo.Spec.CertSecretRef != nil {
		t.Error("the repository's secrets must not be attached to a foreign registry")
	}

	helmChart := &sourcev1.HelmChart{}
	if err := c.Get(context.Background(), key, helmChart); err == nil {
		t.Fatal("no helm chart must be created for a version published in a registry")
	}
}

// TestResolveArchiveVersionUsesHelmChart is the regression guard for the plain case,
// and the one that reproduces what a cluster showed: a version an index publishes as a
// .tgz archive carries neither a media type nor a reference, and that is not a verdict
// about it — the catalog has nothing more to say about such a version. Reading "no
// media type" as "not resolved yet" left every archive version of every helm
// repository reporting pending forever, which is a wider break than the hybrid bug it
// came in with.
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

// TestResolveDispatchesOnTheRequestShape covers what the HTTP layer cannot: an
// unknown kind and a namespaced kind without a namespace are request errors,
// distinguishable from "the repository is gone". A namespace on a cluster-scoped
// kind is not a request error — see TestClusterScopedRequestsIgnoreAStrayNamespace
// for what happens to it instead.
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
			// The spec requires a namespace here for authorization, even though it is a
			// cluster-scoped repository kind: it must not be rejected, and it does not
			// identify the repository, so "example" is still looked up cluster-wide.
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

// TestClusterScopedRequestsIgnoreAStrayNamespace pins the rule the spec draws for
// the request namespace: it is part of a chart's identity only for a namespaced
// family. For a cluster-scoped kind — the addon family, whose contract must not
// move, and the cluster application family, which the caller must authorize through
// a namespace it does not otherwise use — a namespace on the request must resolve to
// the exact same auxiliary resource name and cache entry as a request without one.
// Otherwise the same chart would get a second, per-namespace copy of its internal
// objects for every namespace a caller happens to send.
func TestClusterScopedRequestsIgnoreAStrayNamespace(t *testing.T) {
	for _, kind := range []RepositoryKind{RepositoryKindHelmClusterAddon, RepositoryKindHelmClusterApplication} {
		t.Run(string(kind), func(t *testing.T) {
			meta := metav1.ObjectMeta{Name: "example", UID: "repo-uid", Generation: 2}

			var repo client.Object = &helmv1alpha1.HelmClusterAddonRepository{ObjectMeta: meta}
			if kind == RepositoryKindHelmClusterApplication {
				repo = &helmv1alpha1.HelmClusterApplicationRepository{ObjectMeta: meta}
			}

			r := newTestResolver(t, repo)

			req := Request{Kind: kind, RepositoryName: "example", Chart: "podinfo", Version: "6.7.1"}
			name := cvnaming.AuxResourceName(string(kind), "",
				req.RepositoryName, req.Chart, req.Version, identityOf(meta))
			if err := r.cache.Put(name, []byte("replicaCount: 1\n")); err != nil {
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

// TestReadValuesCachesNothingFromARefusedArchive pins what a refused extraction
// must leave behind: nothing. A cache entry is what every later request is served
// from without any check of its own, so half of an archive that was never accepted
// reaching the disk would outlive the request that fetched it.
func TestReadValuesCachesNothingFromARefusedArchive(t *testing.T) {
	// A few kilobytes on the wire that walk into tens of megabytes: the archive
	// the total budget exists for.
	archive := buildBombArchive(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(archive)
	}))
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	r := &Resolver{
		cache:            cache.New(dir),
		httpClient:       srv.Client(),
		maxArtifactBytes: 100 << 20,
	}

	got, err := r.readValues(context.Background(), "entry", &meta.Artifact{
		URL:    srv.URL,
		Digest: fmt.Sprintf("sha256:%x", sha256.Sum256(archive)),
	})
	if err != nil {
		t.Fatalf("readValues returned %v", err)
	}
	if got.Outcome != OutcomeFetchFailed {
		t.Fatalf("outcome = %q, want fetch_failed (message %q)", got.Outcome, got.Message)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading cache dir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("cache holds %d entries, want none: a refused archive must not be served to the next request", len(entries))
	}
}

// buildBombArchive packs a large, highly compressible file ahead of values.yaml,
// so reaching the values means walking past the budget.
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

// TestAReplacedRepositoryIsNotServedFromTheOldEntry pins what the cache is keyed
// on. A repository name is reusable: delete one and create another under it, or
// point the existing one somewhere else, and the request tuple is unchanged. If
// that tuple were the key, the caller would be handed the values of a chart from a
// source that no longer exists — under a namespaced kind, from another team's
// repository.
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
			if err := r.cache.Put(stale, []byte("replicaCount: 1\n")); err != nil {
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

// TestAStatusWriteDoesNotInvalidateTheEntry is the other side of that key. Both
// repository kinds carry a status subresource the operator writes on every
// synchronization — conditions, the observed generation, the catalog — and none of
// it changes where the chart comes from. Keying on anything that moved with those
// writes would re-download every chart on every sync.
func TestAStatusWriteDoesNotInvalidateTheEntry(t *testing.T) {
	const (
		repoName = "example"
		chart    = "podinfo"
		version  = "6.7.1"
	)

	repo := &helmv1alpha1.HelmClusterAddonRepository{
		ObjectMeta: metav1.ObjectMeta{Name: repoName, UID: "repo-uid", Generation: 1},
	}

	r := newTestResolver(t, repo)

	name := cvnaming.AuxResourceName(string(RepositoryKindHelmClusterAddon), "",
		repoName, chart, version, identityOf(repo.ObjectMeta))
	if err := r.cache.Put(name, []byte("replicaCount: 1\n")); err != nil {
		t.Fatalf("seeding cache: %v", err)
	}

	// What the operator does on a sync: conditions and the observed generation move,
	// the spec does not.
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
