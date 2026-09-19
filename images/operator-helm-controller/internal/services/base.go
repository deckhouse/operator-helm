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

package services

import (
	"context"
	"fmt"
	"time"

	"github.com/fluxcd/pkg/apis/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type BaseService struct {
	Client client.Client
	Scheme *runtime.Scheme
}

func (s *BaseService) ensureResourceDeleted(ctx context.Context, nn types.NamespacedName, obj client.Object) error {
	_, err := s.deleteAndCheck(ctx, nn, obj)
	return err
}

// deleteAndCheck issues a delete for the object if it is still present and reports
// whether it still exists. A deletion may stay pending because a downstream
// controller (helm-controller, source-controller) holds a finalizer and has
// not finished tearing the resource down yet, so callers that must not proceed
// until the resource is actually gone should keep requeuing while exists is true.
func (s *BaseService) deleteAndCheck(ctx context.Context, nn types.NamespacedName, obj client.Object) (exists bool, err error) {
	if err := s.Client.Get(ctx, nn, obj); err != nil {
		return false, client.IgnoreNotFound(err)
	}

	if err := s.Client.Delete(ctx, obj); client.IgnoreNotFound(err) != nil {
		return true, fmt.Errorf("failed to delete resource %s/%s: %w", nn.Namespace, nn.Name, err)
	}

	return true, nil
}

type BaseRepoService struct {
	BaseService

	TargetNamespace string
}

// setReconcileRequestAnnotations stamps the flux reconcile/force request
// annotations so the controller owning obj reconciles it immediately instead of
// waiting for its next interval. Both are stamped with the same timestamp:
// ForceRequestAnnotation is only honoured when it matches
// ReconcileRequestAnnotation.
func setReconcileRequestAnnotations(obj metav1.Object) {
	annotations := obj.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}

	ts := time.Now().UTC().Format(time.RFC3339)
	annotations[meta.ForceRequestAnnotation] = ts
	annotations[meta.ReconcileRequestAnnotation] = ts

	obj.SetAnnotations(annotations)
}
