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

package pass

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
)

func newForceClient(t *testing.T, objects ...client.Object) client.Client {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := helmv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("registering scheme: %v", err)
	}

	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
}

func addonWithAnnotations(annotations map[string]string) *helmv1alpha1.HelmClusterAddon {
	return &helmv1alpha1.HelmClusterAddon{
		ObjectMeta: metav1.ObjectMeta{Name: "addon", Annotations: annotations},
	}
}

func TestConsumeForceAnnotationRemovesTheRequest(t *testing.T) {
	addon := addonWithAnnotations(map[string]string{
		helmv1alpha1.AnnotationForceReconcile: "2026-01-01T00:00:00Z",
		"example.io/unrelated":                "value",
	})
	c := newForceClient(t, addon)
	key := types.NamespacedName{Name: addon.Name}

	if err := ConsumeForceAnnotation(context.Background(), c, key, &helmv1alpha1.HelmClusterAddon{}); err != nil {
		t.Fatalf("ConsumeForceAnnotation returned %v", err)
	}

	stored := &helmv1alpha1.HelmClusterAddon{}
	if err := c.Get(context.Background(), key, stored); err != nil {
		t.Fatalf("getting addon: %v", err)
	}
	if _, found := stored.Annotations[helmv1alpha1.AnnotationForceReconcile]; found {
		t.Fatal("the request must be taken away once it has been acted on")
	}
	if stored.Annotations["example.io/unrelated"] != "value" {
		t.Fatal("unrelated annotations must be left in place")
	}
}

// TestConsumeForceAnnotationSkipsAnObjectWithoutTheRequest pins that an object
// carrying unrelated annotations is not written on every pass. Guarding on the map
// instead of on the annotation itself sends an empty PATCH each time, which costs a
// write and an update event for every object in the cluster.
func TestConsumeForceAnnotationSkipsAnObjectWithoutTheRequest(t *testing.T) {
	addon := addonWithAnnotations(map[string]string{"example.io/unrelated": "value"})
	c := newForceClient(t, addon)
	key := types.NamespacedName{Name: addon.Name}

	stored := &helmv1alpha1.HelmClusterAddon{}
	if err := c.Get(context.Background(), key, stored); err != nil {
		t.Fatalf("getting addon: %v", err)
	}
	before := stored.ResourceVersion

	if err := ConsumeForceAnnotation(context.Background(), c, key, &helmv1alpha1.HelmClusterAddon{}); err != nil {
		t.Fatalf("ConsumeForceAnnotation returned %v", err)
	}

	if err := c.Get(context.Background(), key, stored); err != nil {
		t.Fatalf("getting addon: %v", err)
	}
	if stored.ResourceVersion != before {
		t.Fatalf("resourceVersion moved from %s to %s: an object without the request was written",
			before, stored.ResourceVersion)
	}
}

func TestConsumeForceAnnotationToleratesAMissingObject(t *testing.T) {
	c := newForceClient(t)

	err := ConsumeForceAnnotation(context.Background(), c,
		types.NamespacedName{Name: "gone"}, &helmv1alpha1.HelmClusterAddon{})
	if err != nil {
		t.Fatalf("an object deleted mid-pass must not fail the pass, got %v", err)
	}
}
