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
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
)

func TestMapNamespacedInternalResources(t *testing.T) {
	const target = "d8-operator-helm"

	mapper := MapNamespacedInternalResources(
		"test-controller", target,
		helmv1alpha1.LabelManagedBy, helmv1alpha1.LabelManagedByValue,
		helmv1alpha1.HelmApplicationRepositoryLabelSourceName, helmv1alpha1.LabelSourceNamespace,
	)

	secret := func(namespace string, labels map[string]string) *corev1.Secret {
		return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "internal", Namespace: namespace, Labels: labels}}
	}

	full := map[string]string{
		helmv1alpha1.LabelManagedBy:                           helmv1alpha1.LabelManagedByValue,
		helmv1alpha1.HelmApplicationRepositoryLabelSourceName: "stable",
		helmv1alpha1.LabelSourceNamespace:                     "team-a",
	}
	withoutNamespace := map[string]string{
		helmv1alpha1.LabelManagedBy:                           helmv1alpha1.LabelManagedByValue,
		helmv1alpha1.HelmApplicationRepositoryLabelSourceName: "stable",
	}
	withoutName := map[string]string{
		helmv1alpha1.LabelManagedBy:       helmv1alpha1.LabelManagedByValue,
		helmv1alpha1.LabelSourceNamespace: "team-a",
	}
	foreign := map[string]string{
		helmv1alpha1.LabelManagedBy:                           "someone-else",
		helmv1alpha1.HelmApplicationRepositoryLabelSourceName: "stable",
		helmv1alpha1.LabelSourceNamespace:                     "team-a",
	}

	cases := []struct {
		name string
		obj  client.Object
		want []reconcile.Request
	}{
		{
			name: "maps to the namespaced source",
			obj:  secret(target, full),
			want: []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: "team-a", Name: "stable"}}},
		},
		{name: "ignores objects outside the target namespace", obj: secret("team-a", full)},
		{name: "ignores objects managed by someone else", obj: secret(target, foreign)},
		{name: "skips an object without the source name", obj: secret(target, withoutName)},
		{name: "skips an object without the source namespace", obj: secret(target, withoutNamespace)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := mapper(context.Background(), tc.obj)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("requests = %v, want %v", got, tc.want)
			}
		})
	}
}
