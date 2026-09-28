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
	"time"

	"github.com/fluxcd/pkg/apis/meta"
	sourcev1 "github.com/fluxcd/source-controller/api/v1"
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

// conflictRetryDelay is how long a pass waits after a conflicted delete before judging
// an auxiliary resource again.
const conflictRetryDelay = 10 * time.Second

// reconciler re-caches values.yaml on a new artifact revision and deletes a resource once expired.
type reconciler struct {
	client           client.Client
	cache            *cache.Cache
	httpClient       *http.Client
	maxArtifactBytes int64
	newObject        func() client.Object
	artifactOf       artifactFunc
	conditionsOf     conditionsFunc
}

func (r *reconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	logger := log.FromContext(ctx)

	obj := r.newObject()
	if err := r.client.Get(ctx, req.NamespacedName, obj); err != nil {
		if apierrors.IsNotFound(err) {
			_ = r.cache.Delete(req.Name)
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
		return r.deleteExpired(ctx, obj, name)
	}

	r.refreshCache(ctx, obj, name)

	if !hasExpiry {
		return reconcile.Result{}, nil
	}

	// The refresh can outlast the expiry this pass judged to be ahead of it.
	if remaining := time.Until(expiresAt); remaining > 0 {
		return reconcile.Result{RequeueAfter: remaining}, nil
	}

	return r.deleteExpired(ctx, obj, name)
}

func (r *reconciler) deleteExpired(ctx context.Context, obj client.Object, name string) (reconcile.Result, error) {
	uid, resourceVersion := obj.GetUID(), obj.GetResourceVersion()

	err := r.client.Delete(ctx, obj, client.Preconditions{UID: &uid, ResourceVersion: &resourceVersion})
	switch {
	case apierrors.IsConflict(err):
		return reconcile.Result{RequeueAfter: conflictRetryDelay}, nil
	case apierrors.IsNotFound(err):
		// Already gone, by another pass or by a deleted namespace, so its cache entry goes too.
	case err != nil:
		return reconcile.Result{}, fmt.Errorf("deleting %s: %w", name, err)
	}

	_ = r.cache.Delete(name)

	if err == nil {
		log.FromContext(ctx).Info("deleted expired auxiliary resource", "name", name)
	}

	return reconcile.Result{}, nil
}

func (r *reconciler) refreshCache(ctx context.Context, obj client.Object, name string) {
	art := r.artifactOf(obj)
	if art == nil || !isReady(r.conditionsOf(obj)) {
		return
	}

	// The entry records the revision it came from, which the resolver's own writes keep true too.
	if entry, ok := r.cache.GetEntry(name); ok && entry.Digest != cache.DigestUnknown && entry.Digest == art.Digest {
		return
	}

	values, err := artifact.FetchValues(ctx, r.httpClient, art.URL, art.Digest, r.maxArtifactBytes)
	if err != nil {
		if !errors.Is(err, artifact.ErrValuesNotFound) {
			log.FromContext(ctx).Error(err, "failed to refresh cached values.yaml", "name", name)
		}

		r.invalidateOutdated(ctx, name, art.Digest)

		return
	}

	if err := r.cache.Put(name, art.Digest, values); err != nil {
		if errors.Is(err, cache.ErrBudgetExhausted) {
			log.FromContext(ctx).Info("not caching values.yaml, cache budget exhausted", "name", name)
		} else {
			log.FromContext(ctx).Error(err, "failed to write cache entry", "name", name)
		}

		r.invalidateOutdated(ctx, name, art.Digest)

		return
	}
}

func (r *reconciler) invalidateOutdated(ctx context.Context, name, digest string) {
	if entry, ok := r.cache.GetEntry(name); ok && entry.Digest != cache.DigestUnknown && entry.Digest == digest {
		return
	}

	if err := r.cache.Delete(name); err != nil {
		log.FromContext(ctx).Error(err, "failed to drop an outdated cache entry", "name", name)
	}
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

// SetupWithManager registers cache-sync and cleanup reconcilers for both auxiliary
// resource kinds, filtered to the resources this controller manages.
func SetupWithManager(mgr manager.Manager, valuesCache *cache.Cache, httpClient *http.Client, maxArtifactBytes int64) error {
	helmChart := &reconciler{
		client:           mgr.GetClient(),
		cache:            valuesCache,
		httpClient:       httpClient,
		maxArtifactBytes: maxArtifactBytes,
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
