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
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/werf/3p-fluxcd-pkg/apis/meta"
	sourcev1 "github.com/werf/nelm-source-controller/api/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/deckhouse/chart-values-controller/internal/artifact"
	"github.com/deckhouse/chart-values-controller/internal/cache"
	"github.com/deckhouse/chart-values-controller/internal/labels"
)

type (
	artifactFunc   func(client.Object) *meta.Artifact
	conditionsFunc func(client.Object) []metav1.Condition
)

// reconciler keeps the values cache in sync with an auxiliary source resource
// (HelmChart or OCIRepository) and deletes the resource once its expires-at
// annotation has passed. It downloads and re-caches values.yaml whenever the
// artifact revision changes, so HTTP requests can be served straight from cache.
type reconciler struct {
	client           client.Client
	cache            *cache.Cache
	httpClient       *http.Client
	maxArtifactBytes int64
	newObject        func() client.Object
	artifactOf       artifactFunc
	conditionsOf     conditionsFunc

	mu      sync.Mutex
	digests map[string]string
}

func (r *reconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	logger := log.FromContext(ctx)

	obj := r.newObject()
	if err := r.client.Get(ctx, req.NamespacedName, obj); err != nil {
		if apierrors.IsNotFound(err) {
			// Resource gone (external or TTL deletion): drop its cache entry.
			_ = r.cache.Delete(req.Name)
			r.forgetDigest(req.Name)
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, fmt.Errorf("getting %s: %w", req.Name, err)
	}

	if obj.GetLabels()[labels.ManagedBy] != labels.ManagedByValue {
		return reconcile.Result{}, nil
	}

	name := obj.GetName()

	expiresAt, hasExpiry := parseExpiry(obj, logger)
	if hasExpiry && !time.Now().Before(expiresAt) {
		_ = r.cache.Delete(name)
		r.forgetDigest(name)
		if err := r.client.Delete(ctx, obj); err != nil {
			return reconcile.Result{}, client.IgnoreNotFound(err)
		}
		logger.Info("deleted expired auxiliary resource", "name", name)
		return reconcile.Result{}, nil
	}

	r.refreshCache(ctx, obj, name)

	if hasExpiry {
		return reconcile.Result{RequeueAfter: time.Until(expiresAt)}, nil
	}

	return reconcile.Result{}, nil
}

func (r *reconciler) refreshCache(ctx context.Context, obj client.Object, name string) {
	art := r.artifactOf(obj)
	if art == nil || !isReady(r.conditionsOf(obj)) {
		return
	}

	if r.lastDigest(name) == art.Digest {
		return
	}

	values, err := artifact.FetchValues(ctx, r.httpClient, art.URL, art.Digest, r.maxArtifactBytes)
	if err != nil {
		if !errors.Is(err, artifact.ErrValuesNotFound) {
			log.FromContext(ctx).Error(err, "failed to refresh cached values.yaml", "name", name)
		}
		return
	}

	if err := r.cache.Put(name, values); err != nil {
		log.FromContext(ctx).Error(err, "failed to write cache entry", "name", name)
		return
	}

	r.setLastDigest(name, art.Digest)
}

func (r *reconciler) lastDigest(name string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.digests[name]
}

func (r *reconciler) setLastDigest(name, digest string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.digests[name] = digest
}

func (r *reconciler) forgetDigest(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.digests, name)
}

func parseExpiry(obj client.Object, logger interface{ Error(error, string, ...any) }) (time.Time, bool) {
	raw := obj.GetAnnotations()[labels.ExpiresAtAnnotation]
	if raw == "" {
		return time.Time{}, false
	}

	expiresAt, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		logger.Error(err, "ignoring malformed expires-at annotation", "value", raw)
		return time.Time{}, false
	}

	return expiresAt, true
}

func isReady(conditions []metav1.Condition) bool {
	c := apimeta.FindStatusCondition(conditions, "Ready")
	return c != nil && c.Status == metav1.ConditionTrue
}

// SetupWithManager registers cache-sync/cleanup reconcilers for both auxiliary
// resource kinds, filtered to resources managed by this controller and triggered
// on artifact-revision or expires-at changes.
func SetupWithManager(mgr manager.Manager, valuesCache *cache.Cache, httpClient *http.Client, maxArtifactBytes int64) error {
	helmChart := &reconciler{
		client:           mgr.GetClient(),
		cache:            valuesCache,
		httpClient:       httpClient,
		maxArtifactBytes: maxArtifactBytes,
		digests:          map[string]string{},
		newObject:        func() client.Object { return &sourcev1.HelmChart{} },
		artifactOf:       func(o client.Object) *meta.Artifact { return o.(*sourcev1.HelmChart).Status.Artifact },
		conditionsOf:     func(o client.Object) []metav1.Condition { return o.(*sourcev1.HelmChart).Status.Conditions },
	}

	if err := builder.ControllerManagedBy(mgr).
		Named("chart-values-helmchart").
		For(&sourcev1.HelmChart{}, builder.WithPredicates(changePredicate(helmChart.artifactOf))).
		Complete(helmChart); err != nil {
		return fmt.Errorf("setting up helm chart controller: %w", err)
	}

	ociRepo := &reconciler{
		client:           mgr.GetClient(),
		cache:            valuesCache,
		httpClient:       httpClient,
		maxArtifactBytes: maxArtifactBytes,
		digests:          map[string]string{},
		newObject:        func() client.Object { return &sourcev1.OCIRepository{} },
		artifactOf:       func(o client.Object) *meta.Artifact { return o.(*sourcev1.OCIRepository).Status.Artifact },
		conditionsOf:     func(o client.Object) []metav1.Condition { return o.(*sourcev1.OCIRepository).Status.Conditions },
	}

	if err := builder.ControllerManagedBy(mgr).
		Named("chart-values-ocirepository").
		For(&sourcev1.OCIRepository{}, builder.WithPredicates(changePredicate(ociRepo.artifactOf))).
		Complete(ociRepo); err != nil {
		return fmt.Errorf("setting up oci repository controller: %w", err)
	}

	return nil
}

// changePredicate limits reconciliation to managed resources and, for updates,
// to changes that matter here: a new artifact revision (content changed) or a
// refreshed expires-at (TTL extended).
func changePredicate(artifactOf artifactFunc) predicate.Predicate {
	managed := func(o client.Object) bool {
		return o.GetLabels()[labels.ManagedBy] == labels.ManagedByValue
	}

	digest := func(o client.Object) string {
		if a := artifactOf(o); a != nil {
			return a.Digest
		}
		return ""
	}

	return predicate.Funcs{
		CreateFunc:  func(e event.CreateEvent) bool { return managed(e.Object) },
		DeleteFunc:  func(e event.DeleteEvent) bool { return managed(e.Object) },
		GenericFunc: func(e event.GenericEvent) bool { return managed(e.Object) },
		UpdateFunc: func(e event.UpdateEvent) bool {
			if !managed(e.ObjectNew) {
				return false
			}
			if digest(e.ObjectOld) != digest(e.ObjectNew) {
				return true
			}
			return e.ObjectOld.GetAnnotations()[labels.ExpiresAtAnnotation] !=
				e.ObjectNew.GetAnnotations()[labels.ExpiresAtAnnotation]
		},
	}
}
