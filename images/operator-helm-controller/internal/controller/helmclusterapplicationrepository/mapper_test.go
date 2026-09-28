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

package helmclusterapplicationrepository

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

func internalSecret(namespace string, labels map[string]string) client.Object {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "derived-name", Namespace: namespace, Labels: labels},
	}
}

func TestMapInternalResourcesReachesTheClusterRepositoryThatOwnsTheObject(t *testing.T) {
	ours := map[string]string{
		helmv1alpha1.LabelManagedBy:                                  helmv1alpha1.LabelManagedByValue,
		helmv1alpha1.HelmClusterApplicationRepositoryLabelSourceName: "stable",
	}
	namespacedApplication := map[string]string{
		helmv1alpha1.LabelManagedBy:                           helmv1alpha1.LabelManagedByValue,
		helmv1alpha1.HelmApplicationRepositoryLabelSourceName: "stable",
		helmv1alpha1.LabelSourceNamespace:                     "team-a",
	}
	clusterAddon := map[string]string{
		helmv1alpha1.LabelManagedBy:                            helmv1alpha1.LabelManagedByValue,
		helmv1alpha1.HelmClusterAddonRepositoryLabelSourceName: "stable",
	}

	tests := []struct {
		name string
		obj  client.Object
		want []reconcile.Request
	}{
		{
			name: "an object of this kind",
			obj:  internalSecret(helmv1alpha1.TargetNamespace, ours),
			want: []reconcile.Request{{NamespacedName: types.NamespacedName{Name: "stable"}}},
		},
		{
			name: "an object of the namespaced application repository kind",
			obj:  internalSecret(helmv1alpha1.TargetNamespace, namespacedApplication),
		},
		{
			name: "an object of the addon repository kind",
			obj:  internalSecret(helmv1alpha1.TargetNamespace, clusterAddon),
		},
		{
			name: "an object outside the operator namespace",
			obj:  internalSecret("team-a", ours),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := mapInternalResources()(context.Background(), tt.obj)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("requests = %v, want %v", got, tt.want)
			}
		})
	}
}
