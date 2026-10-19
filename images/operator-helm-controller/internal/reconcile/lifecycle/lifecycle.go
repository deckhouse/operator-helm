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

// Package lifecycle holds the steps a reconcile pass takes around the object's own lifecycle:
// consuming the force reconcile annotation and waiting on an internal resource's deletion.
package lifecycle

import (
	"context"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
	"github.com/deckhouse/operator-helm/internal/status"
)

// InternalResourceDeletionRequeueInterval bounds how often a pass re-checks whether the
// internal resources have finished being deleted, for one that stops emitting events.
const InternalResourceDeletionRequeueInterval = 30 * time.Second

// RemoveForceAnnotation removes the force reconcile annotation from the object at key,
// re-reading it into obj rather than patching from the copy the pass started with.
func RemoveForceAnnotation(ctx context.Context, c client.Client, key client.ObjectKey, obj client.Object) error {
	if err := c.Get(ctx, key, obj); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}

		return fmt.Errorf("getting object: %w", err)
	}

	annotations := obj.GetAnnotations()
	if _, found := annotations[helmv1alpha1.AnnotationForceReconcile]; !found {
		// Guarded on the annotation, not the map: any other annotation would force an empty PATCH.
		return nil
	}

	patchBase := client.MergeFrom(obj.DeepCopyObject().(client.Object))

	delete(annotations, helmv1alpha1.AnnotationForceReconcile)
	obj.SetAnnotations(annotations)

	if err := c.Patch(ctx, obj, patchBase); err != nil {
		return fmt.Errorf("removing force reconcile annotation: %w", err)
	}

	return nil
}

// MarkPending is the status write that says an internal resource is still going away.
type MarkPending func(ctx context.Context, obj status.ObjectWithConditions, resourceName string, resource status.DeletingResource) error

// AwaitInternalResourceDeletion surfaces that an internal resource is still being deleted on
// the owner's status and requeues without removing the finalizer.
func AwaitInternalResourceDeletion(
	ctx context.Context,
	mark MarkPending,
	owner status.ObjectWithConditions,
	name string,
	resource status.DeletingResource,
) (reconcile.Result, error) {
	log.FromContext(ctx).Info("Waiting for internal resource to be deleted before removing finalizer",
		"resource", name,
		"internalType", fmt.Sprintf("%T", resource),
		"internalObject", client.ObjectKeyFromObject(resource))

	if err := mark(ctx, owner, name, resource); client.IgnoreNotFound(err) != nil {
		return reconcile.Result{}, fmt.Errorf("updating deletion status: %w", err)
	}

	return reconcile.Result{RequeueAfter: InternalResourceDeletionRequeueInterval}, nil
}
