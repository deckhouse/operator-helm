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
	"bytes"
	"context"
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

const (
	testNamespace   = "d8-operator-helm"
	testName        = "chart-values-abc123"
	otherTestName   = "chart-values-def456"
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
		client:    c,
		namespace: testNamespace,
		cache:     cache.New(t.TempDir(), testCacheBudget),
		newObject: func() client.Object { return &sourcev1.HelmChart{} },
	}
}

func reconcileOnce(t *testing.T, r *reconciler) (reconcile.Result, error) {
	t.Helper()

	return r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Namespace: testNamespace, Name: testName},
	})
}

func TestReconcileDeletesOnlyTheExpiredObjectItRead(t *testing.T) {
	past := time.Now().Add(-time.Minute)
	future := time.Now().Add(time.Hour)

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
			if err := r.cache.Put(testName, "sha256:cafe", []byte("replicaCount: 1\n")); err != nil {
				t.Fatalf("seeding cache: %v", err)
			}

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
				if result.RequeueAfter <= 0 {
					t.Fatalf("a refused delete must requeue, got %+v", result)
				}
				if entry, _ := r.cache.GetEntry(testName); entry.Digest != "sha256:cafe" {
					t.Fatalf("the surviving entry records %q, want the revision it was seeded with", entry.Digest)
				}
			}
		})
	}
}

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
	if err := r.cache.Put(testName, "sha256:cafe", []byte("replicaCount: 1\n")); err != nil {
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

func TestReconcileLeavesTheEntryAloneForAnObjectInAnotherNamespace(t *testing.T) {
	const otherNamespace = "tenant-namespace"

	earlier := []byte("replicaCount: 1\n")

	tests := []struct {
		name   string
		client func(t *testing.T) client.Client
	}{
		{
			name: "an expired object",
			client: func(t *testing.T) client.Client {
				t.Helper()

				chart := expiredChart(time.Now().Add(-time.Minute))
				chart.Namespace = otherNamespace
				chart.Status.Artifact = &meta.Artifact{Digest: "sha256:newer"}

				return fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(chart).Build()
			},
		},
		{
			name: "an object that is gone",
			client: func(t *testing.T) client.Client {
				t.Helper()

				return fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newTestReconciler(t, tt.client(t))
			if err := r.cache.Put(testName, "sha256:earlier", earlier); err != nil {
				t.Fatalf("seeding cache: %v", err)
			}

			if _, err := r.Reconcile(context.Background(), reconcile.Request{
				NamespacedName: types.NamespacedName{Namespace: otherNamespace, Name: testName},
			}); err != nil {
				t.Fatalf("reconcile: %v", err)
			}

			got, ok := r.cache.Get(testName)
			if !ok {
				t.Fatal("the entry was deleted for an object outside the served namespace")
			}
			if !bytes.Equal(got, earlier) {
				t.Fatalf("entry = %q, want the seeded %q", got, earlier)
			}
		})
	}
}

func TestReconcileLeavesTheEntryAloneWhenTheArtifactRevisionMoves(t *testing.T) {
	chart := expiredChart(time.Now().Add(time.Hour))
	chart.Status.Artifact = &meta.Artifact{Digest: "sha256:newer"}
	chart.Status.Conditions = []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue}}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(chart).Build()

	r := newTestReconciler(t, c)
	if err := r.cache.Put(testName, "sha256:earlier", []byte("replicaCount: 1\n")); err != nil {
		t.Fatalf("seeding cache: %v", err)
	}

	result, err := reconcileOnce(t, r)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if result.RequeueAfter <= 0 || result.RequeueAfter > time.Hour {
		t.Fatalf("requeue = %v, want the time left until expiry", result.RequeueAfter)
	}
	if _, ok := r.cache.Get(testName); !ok {
		t.Fatal("the controller must not touch an entry whose object is alive; the resolver re-reads the revision itself")
	}
}
