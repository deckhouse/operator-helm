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

package helmapplicationrepository

import (
	"context"
	"reflect"
	"testing"

	sourcev1 "github.com/fluxcd/source-controller/api/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
)

func internalRepository(namespace string, labels map[string]string) client.Object {
	return &sourcev1.HelmRepository{
		ObjectMeta: metav1.ObjectMeta{Name: "derived-name", Namespace: namespace, Labels: labels},
	}
}

func TestMapInternalResourcesReachesTheNamespacedRepositoryThatOwnsTheObject(t *testing.T) {
	ours := map[string]string{
		helmv1alpha1.LabelManagedBy:                           helmv1alpha1.LabelManagedByValue,
		helmv1alpha1.HelmApplicationRepositoryLabelSourceName: "stable",
		helmv1alpha1.LabelSourceNamespace:                     "team-a",
	}
	clusterApplication := map[string]string{
		helmv1alpha1.LabelManagedBy:                                  helmv1alpha1.LabelManagedByValue,
		helmv1alpha1.HelmClusterApplicationRepositoryLabelSourceName: "stable",
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
			obj:  internalRepository(helmv1alpha1.TargetNamespace, ours),
			want: []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: "team-a", Name: "stable"}}},
		},
		{
			name: "an object of the cluster application repository kind",
			obj:  internalRepository(helmv1alpha1.TargetNamespace, clusterApplication),
		},
		{
			name: "an object of the addon repository kind",
			obj:  internalRepository(helmv1alpha1.TargetNamespace, clusterAddon),
		},
		{
			name: "an object outside the operator namespace",
			obj:  internalRepository("team-a", ours),
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
