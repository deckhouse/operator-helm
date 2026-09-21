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

// Package pass holds what one reconcile pass does the same way whichever kind it
// runs on. Both reconcilers consume the force reconcile annotation and both wait
// for an internal resource to finish being deleted; neither needs to know which
// family the object belongs to, so neither is written twice.
package pass

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

// InternalResourceDeletionRequeueInterval bounds how often a pass re-checks whether
// the internal resources have finished being deleted. Watches on those resources
// drive most requeues; this is the safety net for a resource whose deletion is stuck
// and stops emitting events.
const InternalResourceDeletionRequeueInterval = 30 * time.Second

// ConsumeForceAnnotation removes the force reconcile annotation from the object at
// key. obj is an empty object of the right kind to read into: the object is fetched
// again rather than patched from the copy the pass has held since it started, so a
// change made meanwhile is not written back over.
func ConsumeForceAnnotation(ctx context.Context, c client.Client, key client.ObjectKey, obj client.Object) error {
	if err := c.Get(ctx, key, obj); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}

		return fmt.Errorf("getting object: %w", err)
	}

	annotations := obj.GetAnnotations()
	if _, found := annotations[helmv1alpha1.AnnotationForceReconcile]; !found {
		// Guard on the annotation itself, not on the map: an object carrying any
		// unrelated annotation would otherwise take an empty PATCH on every pass.
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

// MarkPending is the status write that says an internal resource is still going
// away: MarkDeletionPending for an owner with nothing to uninstall, and
// MarkUninstallPending for one whose Helm release is being removed.
type MarkPending func(ctx context.Context, obj status.ObjectWithConditions, resourceName string, resource status.DeletingResource) error

// AwaitInternalResourceDeletion surfaces that an internal resource is still being
// deleted on the owner's status and requeues without removing the finalizer. name is
// kept abstract so the internal type is not leaked to the user; the log line names
// the object itself, which is what someone looking into a stuck deletion reaches for.
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
