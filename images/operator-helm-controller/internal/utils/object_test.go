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

package utils

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// TestDeleteIfPresent pins the distinction the callers requeue on: exists reports
// whether the object was still there when the delete was issued, not whether the
// delete finished. A downstream controller holding a finalizer keeps the object
// present, and the caller has to be told to come back for it.
func TestDeleteIfPresent(t *testing.T) {
	nn := types.NamespacedName{Namespace: "d8-operator-helm", Name: "leftover"}
	present := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Namespace:  nn.Namespace,
		Name:       nn.Name,
		Finalizers: []string{"helm.deckhouse.io/cleanup"},
	}}

	c := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).WithObjects(present).Build()

	exists, err := DeleteIfPresent(context.Background(), c, nn, &corev1.Secret{})
	if err != nil {
		t.Fatalf("DeleteIfPresent returned %v", err)
	}
	if !exists {
		t.Fatal("an object held by a finalizer must be reported as still present")
	}

	held := &corev1.Secret{}
	if err := c.Get(context.Background(), nn, held); err != nil {
		t.Fatalf("getting the object a finalizer holds: %v", err)
	}
	if held.DeletionTimestamp.IsZero() {
		t.Fatal("the delete was not issued")
	}

	exists, err = DeleteIfPresent(context.Background(), c, types.NamespacedName{
		Namespace: nn.Namespace, Name: "never-created",
	}, &corev1.Secret{})
	if err != nil {
		t.Fatalf("DeleteIfPresent returned %v for a missing object", err)
	}
	if exists {
		t.Fatal("an object that is not there must not be reported as present")
	}
}
