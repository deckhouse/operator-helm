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

// conflictRetryDelay is how long a pass waits before judging an auxiliary resource
// again after its delete was refused. The write that refused it may well have been
// filtered out by the predicate (a status change that moved neither the artifact
// digest nor expires-at), so waiting for an event instead could wait forever.
const conflictRetryDelay = 10 * time.Second

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

	if hasExpiry {
		return reconcile.Result{RequeueAfter: time.Until(expiresAt)}, nil
	}

	return reconcile.Result{}, nil
}

// deleteExpired removes the exact auxiliary resource this pass read and judged
// expired. Between that read and here the resolver may have extended the annotation
// or recreated the object under the same name for a new request, so the delete
// carries the read's identity as a precondition: the API server then refuses to take
// away an object somebody is already waiting on. A refusal is an ordinary outcome of
// the pass, not a failure — the next pass reads the object again and judges it anew.
//
// The cache entry is dropped only once the object is really gone. Dropping it first
// would leave a live object without its values and every request for it answered
// with a retry until they are downloaded again.
func (r *reconciler) deleteExpired(ctx context.Context, obj client.Object, name string) (reconcile.Result, error) {
	uid, resourceVersion := obj.GetUID(), obj.GetResourceVersion()

	err := r.client.Delete(ctx, obj, client.Preconditions{UID: &uid, ResourceVersion: &resourceVersion})
	switch {
	case apierrors.IsConflict(err):
		return reconcile.Result{RequeueAfter: conflictRetryDelay}, nil
	case apierrors.IsNotFound(err):
		// Already gone, by another pass or by a deleted namespace: nothing is waiting
		// on it, so its cache entry goes too.
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

	// What may be skipped is a key that already holds this revision, and the entry is
	// what says so: it records the digest it was extracted from. Remembering instead
	// what this process last wrote would gate on the wrong fact — the resolver caches
	// on every miss too, and a download it started before this revision was published
	// can land after this pass, leaving an earlier revision under a key this
	// controller believes it made current. The entry is the only statement about what
	// a request would actually be served, and it survives a restart besides.
	if entry, ok := r.cache.GetEntry(name); ok && entry.Digest != cache.DigestUnknown && entry.Digest == art.Digest {
		return
	}

	values, err := artifact.FetchValues(ctx, r.httpClient, art.URL, art.Digest, r.maxArtifactBytes)
	if err != nil {
		if !errors.Is(err, artifact.ErrValuesNotFound) {
			log.FromContext(ctx).Error(err, "failed to refresh cached values.yaml", "name", name)
		}

		// The artifact moved and this pass could not read the new revision — the
		// network, a refused size, or a republish that dropped values.yaml. What is
		// under the key is then judged exactly as it is on a refused write: the
		// failure says nothing about the entry, only that it was not replaced.
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

// invalidateOutdated settles what stays under one key whose refresh could not be
// completed, whatever stopped it. The artifact has moved off whatever the entry
// holds unless the entry itself says otherwise, so serving it would answer a request
// for the chart's values with an earlier revision's, and the response carries nothing
// that would let the caller notice. Dropping the entry costs the next request a
// download — it misses, the resolver prepares the artifact and the client is either
// served the current values or told to retry — which is the honest answer, and it
// gives the budget back the room the write needed.
//
// Which revision the entry holds is not guessed: the entry records the digest it was
// extracted from, and both writers (this controller and the resolver, which caches on
// every miss) record it. An entry already holding the revision being refreshed is
// current, so it stays, or a tight budget would have every pass throw away what the
// previous request cached.
//
// An entry whose digest is unknown can only come from a container image older than
// this format, since every write since records one. Nothing attributes it to the
// current revision and nothing short of a download could, so it goes: once, after
// which the next request has the resolver write it back with its revision named.
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

// SetupWithManager registers cache-sync/cleanup reconcilers for both auxiliary
// resource kinds, filtered to resources managed by this controller and triggered
// on artifact-revision or expires-at changes.
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
