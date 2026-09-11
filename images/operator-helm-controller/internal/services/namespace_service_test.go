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
	"testing"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/deckhouse/operator-helm/internal/adapter"
)

// TestEnsureTargetNamespaceCreatesItOnceAndLeavesItAlone pins the addon rule moved
// here from the reconciler: a missing target namespace is created, an existing one
// — labels included — is not touched.
func TestEnsureTargetNamespaceCreatesItOnceAndLeavesItAlone(t *testing.T) {
	addon := testAddon()
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	service := NewNamespaceService(c)

	if err := service.EnsureTargetNamespace(context.Background(), adapter.NewAddonRelease(addon)); err != nil {
		t.Fatalf("EnsureTargetNamespace returned %v", err)
	}

	ns := &corev1.Namespace{}
	if err := c.Get(context.Background(), client.ObjectKey{Name: "app"}, ns); err != nil {
		t.Fatalf("target namespace was not created: %v", err)
	}

	ns.Labels = map[string]string{"owner": "team"}
	if err := c.Update(context.Background(), ns); err != nil {
		t.Fatalf("labelling namespace: %v", err)
	}

	if err := service.EnsureTargetNamespace(context.Background(), adapter.NewAddonRelease(addon)); err != nil {
		t.Fatalf("second EnsureTargetNamespace returned %v", err)
	}

	again := &corev1.Namespace{}
	if err := c.Get(context.Background(), client.ObjectKey{Name: "app"}, again); err != nil {
		t.Fatalf("getting namespace: %v", err)
	}
	if again.Labels["owner"] != "team" {
		t.Fatalf("an existing namespace must not be rewritten, labels = %v", again.Labels)
	}
}
