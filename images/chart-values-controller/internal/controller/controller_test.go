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

package controller

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/fluxcd/pkg/apis/meta"
	sourcev1 "github.com/fluxcd/source-controller/api/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/deckhouse/chart-values-controller/internal/cache"
	"github.com/deckhouse/chart-values-controller/internal/labels"
)

// cacheBlock is what one entry costs the cache's budget however few bytes it
// holds — the filesystem block the cache rounds to.
const cacheBlock = 4096

const (
	testNamespace = "d8-operator-helm"
	testName      = "chart-values-abc123"
	// otherTestName is a second chart's entry in the same cache directory, there to
	// be found untouched by work about testName.
	otherTestName = "chart-values-def456"
	// testCacheBudget is roomy enough that no test here runs into it; the budget
	// itself is exercised in the cache package.
	testCacheBudget = int64(16 << 20)
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := sourcev1.AddToScheme(scheme); err != nil {
		t.Fatalf("registering source scheme: %v", err)
	}

	return scheme
}

// testUID is the identity of the object every test starts from, named so that a
// delete can be checked for carrying it.
const testUID = types.UID("original-uid")

func expiredChart(expiresAt time.Time) *sourcev1.HelmChart {
	return &sourcev1.HelmChart{
		ObjectMeta: metav1.ObjectMeta{
			Name:        testName,
			Namespace:   testNamespace,
			UID:         testUID,
			Labels:      map[string]string{labels.ManagedBy: labels.ManagedByValue},
			Annotations: map[string]string{labels.ExpiresAtAnnotation: expiresAt.UTC().Format(time.RFC3339)},
		},
	}
}

func newTestReconciler(t *testing.T, c client.Client) *reconciler {
	t.Helper()

	return &reconciler{
		client:       c,
		cache:        cache.New(t.TempDir(), testCacheBudget),
		digests:      map[string]string{},
		newObject:    func() client.Object { return &sourcev1.HelmChart{} },
		artifactOf:   func(o client.Object) *meta.Artifact { return o.(*sourcev1.HelmChart).Status.Artifact },
		conditionsOf: func(o client.Object) []metav1.Condition { return o.(*sourcev1.HelmChart).Status.Conditions },
	}
}

func reconcileOnce(t *testing.T, r *reconciler) (reconcile.Result, error) {
	t.Helper()

	return r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Namespace: testNamespace, Name: testName},
	})
}

// TestReconcileDeletesOnlyTheExpiredObjectItRead pins the collection of expired
// auxiliary resources against the window between reading an object and deleting it.
// A resolver serving a new request extends the annotation or recreates the object
// under the same name; collecting that object would take the source away from a
// request already waiting on it, and drop values the resolver just cached.
func TestReconcileDeletesOnlyTheExpiredObjectItRead(t *testing.T) {
	past := time.Now().Add(-time.Minute)
	future := time.Now().Add(time.Hour)

	// mutate runs once, after the reconciler's read of the object, against the same
	// backing store — exactly where a concurrent resolver would write.
	tests := []struct {
		name    string
		mutate  func(t *testing.T, cl client.WithWatch)
		survive bool
	}{
		{
			name:    "an object nobody touched is deleted with its cache entry",
			mutate:  func(*testing.T, client.WithWatch) {},
			survive: false,
		},
		{
			name: "an object whose expiry was extended after the read survives",
			mutate: func(t *testing.T, cl client.WithWatch) {
				t.Helper()

				chart := &sourcev1.HelmChart{}
				if err := cl.Get(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: testName}, chart); err != nil {
					t.Fatalf("reading chart to extend: %v", err)
				}
				chart.Annotations[labels.ExpiresAtAnnotation] = future.UTC().Format(time.RFC3339)
				if err := cl.Update(context.Background(), chart); err != nil {
					t.Fatalf("extending chart: %v", err)
				}
			},
			survive: true,
		},
		{
			name: "an object recreated under the same name for a new request survives",
			mutate: func(t *testing.T, cl client.WithWatch) {
				t.Helper()

				ctx := context.Background()
				if err := cl.Delete(ctx, expiredChart(past)); err != nil {
					t.Fatalf("deleting chart: %v", err)
				}
				replacement := expiredChart(future)
				replacement.UID = types.UID("replacement-uid")
				if err := cl.Create(ctx, replacement); err != nil {
					t.Fatalf("recreating chart: %v", err)
				}
			},
			survive: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var mutated bool
			c := fake.NewClientBuilder().
				WithScheme(testScheme(t)).
				WithObjects(expiredChart(past)).
				WithInterceptorFuncs(interceptor.Funcs{
					Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
						if err := cl.Get(ctx, key, obj, opts...); err != nil {
							return err
						}
						if !mutated {
							mutated = true
							tt.mutate(t, cl)
						}
						return nil
					},
				}).
				Build()

			r := newTestReconciler(t, c)
			if err := r.cache.Put(testName, []byte("replicaCount: 1\n")); err != nil {
				t.Fatalf("seeding cache: %v", err)
			}
			r.setLastDigest(testName, "sha256:cafe")

			result, err := reconcileOnce(t, r)
			if err != nil {
				t.Fatalf("reconcile returned an error: %v", err)
			}

			err = c.Get(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: testName}, &sourcev1.HelmChart{})
			switch {
			case tt.survive && err != nil:
				t.Fatalf("object should have survived the collection, got: %v", err)
			case !tt.survive && !apierrors.IsNotFound(err):
				t.Fatalf("object should have been deleted, got: %v", err)
			}

			_, cached := r.cache.Get(testName)
			if cached != tt.survive {
				t.Fatalf("cache entry present = %v, want %v", cached, tt.survive)
			}

			if tt.survive {
				// A refused delete must bring the pass back on its own: the write that
				// refused it may never produce an event the predicate lets through.
				if result.RequeueAfter <= 0 {
					t.Fatalf("a refused delete must requeue, got %+v", result)
				}
				if digest := r.lastDigest(testName); digest != "sha256:cafe" {
					t.Fatalf("remembered digest = %q, want it kept", digest)
				}
			}
		})
	}
}

// TestReconcileNamesTheReadIdentityInTheDelete pins both halves of the
// precondition. The fake client enforces only the resource version
// (client/fake enforces ResourceVersion alone), so the recreation case above
// would still pass if the UID were dropped from the delete — the replacement
// carries a different version anyway. Here the request itself is inspected.
func TestReconcileNamesTheReadIdentityInTheDelete(t *testing.T) {
	var options client.DeleteOptions

	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(expiredChart(time.Now().Add(-time.Minute))).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(
				ctx context.Context,
				cl client.WithWatch,
				obj client.Object,
				opts ...client.DeleteOption,
			) error {
				options = client.DeleteOptions{}
				options.ApplyOptions(opts)

				return cl.Delete(ctx, obj, opts...)
			},
		}).
		Build()

	if _, err := reconcileOnce(t, newTestReconciler(t, c)); err != nil {
		t.Fatalf("reconcile returned an error: %v", err)
	}

	if options.Preconditions == nil {
		t.Fatal("the delete named no identity at all")
	}
	if uid := options.Preconditions.UID; uid == nil || *uid != testUID {
		t.Fatalf("delete precondition UID = %v, want %q", uid, testUID)
	}
	if version := options.Preconditions.ResourceVersion; version == nil || *version == "" {
		t.Fatalf("delete precondition resource version = %v, want the one that was read", version)
	}
}

// TestReconcileTreatsAnAlreadyDeletedObjectAsCollected covers the object that is gone
// by the time the delete runs: another pass, or a namespace teardown, got there first.
// There is nothing left to wait on, so the pass completes instead of failing.
func TestReconcileTreatsAnAlreadyDeletedObjectAsCollected(t *testing.T) {
	var mutated bool
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(expiredChart(time.Now().Add(-time.Minute))).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if err := cl.Get(ctx, key, obj, opts...); err != nil {
					return err
				}
				if !mutated {
					mutated = true
					if err := cl.Delete(ctx, obj.DeepCopyObject().(client.Object)); err != nil {
						t.Fatalf("deleting chart behind the reconciler: %v", err)
					}
				}
				return nil
			},
		}).
		Build()

	r := newTestReconciler(t, c)
	if err := r.cache.Put(testName, []byte("replicaCount: 1\n")); err != nil {
		t.Fatalf("seeding cache: %v", err)
	}

	result, err := reconcileOnce(t, r)
	if err != nil {
		t.Fatalf("a missing object is not a failure of the pass, got: %v", err)
	}
	if result.RequeueAfter != 0 {
		t.Fatalf("nothing to come back for, got %+v", result)
	}
	if _, cached := r.cache.Get(testName); cached {
		t.Fatal("cache entry of a deleted object should be dropped")
	}
}

// TestARefusedRefreshNeverLeavesAnEarlierRevisionUnderTheKey pins what a pass
// whose cache write was refused leaves behind. The entry under the key was cached
// for some earlier moment of this chart and version, and the artifact has moved
// since; answering a request for the chart's default values with the previous
// revision's is worse than not answering with values at all, because the response
// says nothing that would let the caller tell. A miss costs one more round trip
// and is either served the current values or told to retry.
//
// The exception is the entry that already holds the very revision being refreshed
// — the resolver caches one on every miss and keeps no digests — which is current
// and must survive, or a tight budget would have every pass throw away what the
// previous request cached.
func TestARefusedRefreshNeverLeavesAnEarlierRevisionUnderTheKey(t *testing.T) {
	current := []byte("replicaCount: 2\n")
	earlier := []byte("replicaCount: 1\n")

	cases := []struct {
		name string
		// cached is what is on the volume when the refused pass runs.
		cached []byte
		// knownDigest is what this controller remembers having written there, empty
		// for the entry it did not write: the resolver's, or one a restarted
		// container found on the volume with its map empty.
		knownDigest string
		// wantCached is what a read after the pass must find, nil for a miss.
		wantCached []byte
	}{
		{
			name:        "an entry this controller wrote for a revision the artifact has moved off",
			cached:      earlier,
			knownDigest: "sha256:the-previous-revision",
			wantCached:  nil,
		},
		{
			name:       "an entry of unknown provenance holding values that are not the current ones",
			cached:     earlier,
			wantCached: nil,
		},
		{
			name:       "an entry of unknown provenance already holding the revision being refreshed",
			cached:     current,
			wantCached: current,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, digest := newRefreshFixture(t, current)

			// Seeded with room to spare, then reopened over the same directory with
			// room for that entry alone: the refresh stages a temp file beside it,
			// which is a second block and does not fit, so the write is refused.
			dir := t.TempDir()
			r.cache = cache.New(dir, 4*cacheBlock)
			if err := r.cache.Put(testName, tc.cached); err != nil {
				t.Fatalf("seeding the entry: %v", err)
			}
			r.cache = cache.New(dir, cacheBlock)

			if tc.knownDigest != "" {
				r.setLastDigest(testName, tc.knownDigest)
			}

			if _, err := reconcileOnce(t, r); err != nil {
				t.Fatalf("a refused cache write is not a failure of the pass, got: %v", err)
			}

			got, ok := r.cache.Get(testName)
			switch {
			case tc.wantCached == nil:
				if ok {
					t.Fatalf("a read after the refused refresh is answered with %q; it must miss so the client is prepared for or told to retry", got)
				}
				if remembered := r.lastDigest(testName); remembered != "" {
					t.Fatalf("digest %q remembered for a key holding nothing: a later pass has to write it again", remembered)
				}
			case !ok:
				t.Fatal("the entry holding the revision being refreshed was thrown away")
			case !bytes.Equal(got, tc.wantCached):
				t.Fatalf("entry = %q, want %q", got, tc.wantCached)
			case r.lastDigest(testName) != digest:
				t.Fatal("the revision that entry holds must be remembered, or every pass under a tight budget refuses and judges it again")
			}
		})
	}
}

// TestARefusedRefreshLeavesTheEntryOfAnotherChartAlone guards the dropping above
// against reaching past the key it is about: the budget is a property of the whole
// directory, so a refusal here is routinely caused by entries of other charts, and
// those entries are current and belong to requests of their own.
func TestARefusedRefreshLeavesTheEntryOfAnotherChartAlone(t *testing.T) {
	current := []byte("replicaCount: 2\n")
	other := []byte("replicaCount: 9\n")

	r, _ := newRefreshFixture(t, current)

	dir := t.TempDir()
	r.cache = cache.New(dir, 8*cacheBlock)
	if err := r.cache.Put(testName, []byte("replicaCount: 1\n")); err != nil {
		t.Fatalf("seeding the entry being refreshed: %v", err)
	}
	if err := r.cache.Put(otherTestName, other); err != nil {
		t.Fatalf("seeding another chart's entry: %v", err)
	}
	// Room for the two entries and nothing more, so the refresh is refused.
	r.cache = cache.New(dir, 2*cacheBlock)
	r.setLastDigest(testName, "sha256:the-previous-revision")

	if _, err := reconcileOnce(t, r); err != nil {
		t.Fatalf("a refused cache write is not a failure of the pass, got: %v", err)
	}

	if got, ok := r.cache.Get(testName); ok {
		t.Fatalf("the refreshed key still answers with %q", got)
	}

	got, ok := r.cache.Get(otherTestName)
	if !ok {
		t.Fatal("another chart's entry was dropped by a refusal it had nothing to do with")
	}
	if !bytes.Equal(got, other) {
		t.Fatalf("another chart's entry = %q, want %q", got, other)
	}
}

// TestRefreshReplacesTheValuesWhenTheArtifactRevisionMoves is the path the refusal
// above is the degradation of: with room on the volume, a republished chart has
// its cached values replaced by the new revision's, and the revision is remembered
// so the passes that follow do not download it again.
func TestRefreshReplacesTheValuesWhenTheArtifactRevisionMoves(t *testing.T) {
	current := []byte("replicaCount: 2\n")

	r, digest := newRefreshFixture(t, current)
	if err := r.cache.Put(testName, []byte("replicaCount: 1\n")); err != nil {
		t.Fatalf("seeding the previous revision's entry: %v", err)
	}
	r.setLastDigest(testName, "sha256:the-previous-revision")

	if _, err := reconcileOnce(t, r); err != nil {
		t.Fatalf("refresh pass: %v", err)
	}

	got, ok := r.cache.Get(testName)
	if !ok {
		t.Fatal("the refreshed values were not cached")
	}
	if !bytes.Equal(got, current) {
		t.Fatalf("entry = %q, want the current revision's values %q", got, current)
	}
	if r.lastDigest(testName) != digest {
		t.Fatalf("lastDigest = %q, want the revision just written %q", r.lastDigest(testName), digest)
	}
}

// newRefreshFixture is a reconciler pointed at a chart whose artifact is published
// and readable — what makes a pass attempt a refresh at all — serving an archive
// that carries the given values. It returns the artifact's digest, which is the
// revision a successful refresh records.
func newRefreshFixture(t *testing.T, values []byte) (*reconciler, string) {
	t.Helper()

	archive := buildValuesArchive(t, values)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(archive)
	}))
	t.Cleanup(srv.Close)

	digest := fmt.Sprintf("sha256:%x", sha256.Sum256(archive))
	chart := readyChartWithArtifact(srv.URL, digest)

	r := newTestReconciler(t, fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(chart).Build())
	r.httpClient = srv.Client()
	r.maxArtifactBytes = 1 << 20

	return r, digest
}

// TestARefusedRefreshDeletesAtMostOnce follows the sequence the deletion above
// starts: the stale entry goes, the next request has the resolver cache the
// current values under the same key, and the pass after that is refused too. If
// the controller still remembered the digest of what it deleted, that fresh entry
// would look stale to it and be deleted in turn — the thrash the previous fix was
// about, one pass further along.
func TestARefusedRefreshDeletesAtMostOnce(t *testing.T) {
	values := []byte("replicaCount: 2\n")
	archive := buildValuesArchive(t, values)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(archive)
	}))
	t.Cleanup(srv.Close)

	chart := readyChartWithArtifact(srv.URL, fmt.Sprintf("sha256:%x", sha256.Sum256(archive)))
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(chart).Build()

	r := newTestReconciler(t, c)
	r.httpClient = srv.Client()
	r.maxArtifactBytes = 1 << 20

	dir := t.TempDir()
	r.cache = cache.New(dir, 4*cacheBlock)
	if err := r.cache.Put(testName, []byte("replicaCount: 1\n")); err != nil {
		t.Fatalf("seeding the stale entry: %v", err)
	}
	r.cache = cache.New(dir, cacheBlock)
	r.setLastDigest(testName, "sha256:the-previous-revision")

	if _, err := reconcileOnce(t, r); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	if _, ok := r.cache.Get(testName); ok {
		t.Fatal("the stale entry should have been dropped by the first pass")
	}

	// What the resolver does on the miss that follows: the values it just served
	// are cached under the same key. A write needs room for the temp file beside
	// the entry, so this is the budget with the space the deletion freed; it is
	// then reopened at the tight one, which is where the next pass runs.
	r.cache = cache.New(dir, 2*cacheBlock)
	if err := r.cache.Put(testName, values); err != nil {
		t.Fatalf("the resolver could not cache the current values: %v", err)
	}
	r.cache = cache.New(dir, cacheBlock)

	if _, err := reconcileOnce(t, r); err != nil {
		t.Fatalf("second pass: %v", err)
	}

	got, ok := r.cache.Get(testName)
	if !ok {
		t.Fatal("the current values cached by the resolver were deleted by a later refused pass")
	}
	if !bytes.Equal(got, values) {
		t.Fatalf("entry = %q, want the current values %q", got, values)
	}
}

// readyChartWithArtifact is a chart whose artifact is published and ready to be
// read, which is what makes a pass attempt a refresh at all.
func readyChartWithArtifact(url, digest string) *sourcev1.HelmChart {
	return &sourcev1.HelmChart{
		ObjectMeta: metav1.ObjectMeta{
			Name:        testName,
			Namespace:   testNamespace,
			Labels:      map[string]string{labels.ManagedBy: labels.ManagedByValue},
			Annotations: map[string]string{labels.ExpiresAtAnnotation: time.Now().Add(time.Hour).UTC().Format(time.RFC3339)},
		},
		Status: sourcev1.HelmChartStatus{
			Artifact:   &meta.Artifact{URL: url, Digest: digest},
			Conditions: []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue, Reason: "Succeeded"}},
		},
	}
}

// buildValuesArchive packs values.yaml the way a chart artifact carries it.
func buildValuesArchive(t *testing.T, values []byte) []byte {
	t.Helper()

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	if err := tw.WriteHeader(&tar.Header{
		Name: "podinfo/values.yaml",
		Mode: 0o644,
		Size: int64(len(values)),
	}); err != nil {
		t.Fatalf("writing archive header: %v", err)
	}
	if _, err := tw.Write(values); err != nil {
		t.Fatalf("writing archive entry: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("closing tar writer: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("closing gzip writer: %v", err)
	}

	return buf.Bytes()
}
