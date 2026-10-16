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
	"fmt"
	"time"

	sourcev1 "github.com/fluxcd/source-controller/api/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/deckhouse/chart-values-controller/internal/cache"
	"github.com/deckhouse/chart-values-controller/internal/labels"
)

// conflictRetryDelay is how long a pass waits after a conflicted delete before judging
// an auxiliary resource again.
const conflictRetryDelay = 10 * time.Second

// reconciler deletes a resource once expired, together with its cache entry.
type reconciler struct {
	client    client.Client
	namespace string
	cache     *cache.Cache
	newObject func() client.Object
}

func (r *reconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	// The cache is keyed by name alone, so only the served namespace may touch it.
	if req.Namespace != r.namespace {
		return reconcile.Result{}, nil
	}

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

	expiresAt, hasExpiry := parseExpiry(obj, logger)
	if !hasExpiry {
		return reconcile.Result{}, nil
	}

	if remaining := time.Until(expiresAt); remaining > 0 {
		return reconcile.Result{RequeueAfter: remaining}, nil
	}

	return r.deleteExpired(ctx, obj, obj.GetName())
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

// SetupWithManager registers cleanup reconcilers for both auxiliary resource kinds,
// filtered to the resources this controller manages.
func SetupWithManager(mgr manager.Manager, valuesCache *cache.Cache, namespace string) error {
	helmChart := &reconciler{
		client:    mgr.GetClient(),
		namespace: namespace,
		cache:     valuesCache,
		newObject: func() client.Object { return &sourcev1.HelmChart{} },
	}

	if err := builder.ControllerManagedBy(mgr).
		Named("chart-values-helmchart").
		For(&sourcev1.HelmChart{}, builder.WithPredicates(changePredicate())).
		Complete(helmChart); err != nil {
		return fmt.Errorf("setting up helm chart controller: %w", err)
	}

	ociRepo := &reconciler{
		client:    mgr.GetClient(),
		namespace: namespace,
		cache:     valuesCache,
		newObject: func() client.Object { return &sourcev1.OCIRepository{} },
	}

	if err := builder.ControllerManagedBy(mgr).
		Named("chart-values-ocirepository").
		For(&sourcev1.OCIRepository{}, builder.WithPredicates(changePredicate())).
		Complete(ociRepo); err != nil {
		return fmt.Errorf("setting up oci repository controller: %w", err)
	}

	return nil
}

func changePredicate() predicate.Predicate {
	managed := func(o client.Object) bool {
		return o.GetLabels()[labels.ManagedBy] == labels.ManagedByValue
	}

	return predicate.Funcs{
		CreateFunc:  func(e event.CreateEvent) bool { return managed(e.Object) },
		DeleteFunc:  func(e event.DeleteEvent) bool { return managed(e.Object) },
		GenericFunc: func(e event.GenericEvent) bool { return managed(e.Object) },
		UpdateFunc: func(e event.UpdateEvent) bool {
			return managed(e.ObjectNew) &&
				e.ObjectOld.GetAnnotations()[labels.ExpiresAtAnnotation] !=
					e.ObjectNew.GetAnnotations()[labels.ExpiresAtAnnotation]
		},
	}
}
