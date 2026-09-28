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

package namespace

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/deckhouse/operator-helm/internal/adapter"
	"github.com/deckhouse/operator-helm/internal/contract"
)

func TestNamespacesEnsureTargetCreatesItOnceAndLeavesItAlone(t *testing.T) {
	addon := testAddon()
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	service := NewNamespaces(c, c)

	if err := service.EnsureTarget(context.Background(), adapter.NewAddonRelease(addon)); err != nil {
		t.Fatalf("EnsureTarget returned %v", err)
	}

	ns := &corev1.Namespace{}
	if err := c.Get(context.Background(), client.ObjectKey{Name: "app"}, ns); err != nil {
		t.Fatalf("target namespace was not created: %v", err)
	}

	ns.Labels = map[string]string{"owner": "team"}
	if err := c.Update(context.Background(), ns); err != nil {
		t.Fatalf("labelling namespace: %v", err)
	}

	if err := service.EnsureTarget(context.Background(), adapter.NewAddonRelease(addon)); err != nil {
		t.Fatalf("second EnsureTarget returned %v", err)
	}

	again := &corev1.Namespace{}
	if err := c.Get(context.Background(), client.ObjectKey{Name: "app"}, again); err != nil {
		t.Fatalf("getting namespace: %v", err)
	}
	if again.Labels["owner"] != "team" {
		t.Fatalf("an existing namespace must not be rewritten, labels = %v", again.Labels)
	}
}

func TestTerminatingReportsANamespaceThatIsGoingOrAlreadyGone(t *testing.T) {
	deleted := metav1.Now()

	tests := []struct {
		name      string
		namespace *corev1.Namespace
		want      bool
	}{
		{
			name: "a live namespace is not terminating",
			namespace: &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{Name: "app"},
			},
			want: false,
		},
		{
			name: "a namespace with a deletion timestamp is terminating",
			namespace: &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "app",
					Finalizers:        []string{"kubernetes"},
					DeletionTimestamp: &deleted,
				},
			},
			want: true,
		},
		{
			name:      "a namespace that is not there is terminating",
			namespace: nil,
			want:      true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			builder := fake.NewClientBuilder().WithScheme(testScheme(t))
			if tt.namespace != nil {
				builder = builder.WithObjects(tt.namespace)
			}
			c := builder.Build()

			addon := testAddon()
			for name, service := range map[string]interface {
				Terminating(context.Context, contract.Release) (bool, error)
			}{
				"Namespaces":     NewNamespaces(c, c),
				"ExistingTarget": NewExistingTarget(c),
			} {
				got, err := service.Terminating(context.Background(), adapter.NewAddonRelease(addon))
				if err != nil {
					t.Fatalf("%s: Terminating returned %v", name, err)
				}
				if got != tt.want {
					t.Fatalf("%s: Terminating = %v, want %v", name, got, tt.want)
				}
			}
		})
	}
}
