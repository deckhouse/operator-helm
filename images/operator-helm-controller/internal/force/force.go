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

// Package force carries the force reconcile request a user places on a resource with an
// annotation: once a pass has acted on the request, the annotation is removed.
package force

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
)

// RemoveAnnotation removes the force reconcile annotation from the object at key,
// re-reading it into obj rather than patching from the copy the pass started with.
func RemoveAnnotation(ctx context.Context, c client.Client, key client.ObjectKey, obj client.Object) error {
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
