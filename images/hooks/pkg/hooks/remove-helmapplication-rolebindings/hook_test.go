/*
Copyright 2026 Flant JSC

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

package remove_helmapplication_rolebindings

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	"github.com/deckhouse/module-sdk/pkg"
	"github.com/deckhouse/module-sdk/testing/mock"

	"hooks/pkg/settings"
)

type deleteCall struct {
	apiVersion string
	kind       string
	namespace  string
	name       string
}

func newInput(t *testing.T, bindings []resource, workloads ...*unstructured.Unstructured) (*pkg.HookInput, *[]deleteCall) {
	t.Helper()

	calls := make([]deleteCall, 0)

	patchCollector := mock.NewPatchCollectorMock(t)
	patchCollector.DeleteMock.Optional().Set(func(apiVersion, kind, namespace, name string) {
		calls = append(calls, deleteCall{apiVersion, kind, namespace, name})
	})

	snapshotsMock := mock.NewSnapshotsMock(t)
	snapshotsMock.GetMock.Optional().Set(func(key string) []pkg.Snapshot {
		if key != roleBindingSnapshot {
			return nil
		}

		result := make([]pkg.Snapshot, 0, len(bindings))
		for _, res := range bindings {
			result = append(result, snapshotOf(t, res))
		}

		return result
	})

	dc := mock.NewDependencyContainerMock(t)
	dc.GetK8sClientMock.Return(k8sClient{dynamic: fakeDynamicClient(t, workloads...)}, nil)

	return &pkg.HookInput{
		DC:             dc,
		PatchCollector: patchCollector,
		Snapshots:      snapshotsMock,
	}, &calls
}

func snapshotOf(t *testing.T, res resource) pkg.Snapshot {
	t.Helper()

	snapshot := mock.NewSnapshotMock(t)
	snapshot.UnmarshalToMock.Set(func(v any) error {
		raw, err := json.Marshal(res)
		if err != nil {
			return err
		}

		return json.Unmarshal(raw, v)
	})

	return snapshot
}

type k8sClient struct {
	pkg.KubernetesClient

	dynamic dynamic.Interface
}

func (c k8sClient) Dynamic() dynamic.Interface { return c.dynamic }

func fakeDynamicClient(t *testing.T, objects ...*unstructured.Unstructured) dynamic.Interface {
	t.Helper()

	list := &unstructured.UnstructuredList{}
	for _, object := range objects {
		list.Items = append(list.Items, *object)
	}

	resourceMock := mock.NewKubernetesNamespaceableResourceInterfaceMock(t)
	resourceMock.NamespaceMock.Optional().Return(resourceMock)
	resourceMock.ListMock.Optional().Set(
		func(_ context.Context, _ metav1.ListOptions) (*unstructured.UnstructuredList, error) {
			return list, nil
		},
	)

	dynamicClient := mock.NewKubernetesDynamicClientMock(t)
	dynamicClient.ResourceMock.Optional().Set(
		func(_ schema.GroupVersionResource) dynamic.NamespaceableResourceInterface { return resourceMock },
	)

	return dynamicClient
}

func TestRemoveRoleBindingsDeletesEveryBindingInItsOwnNamespace(t *testing.T) {
	bindings := []resource{
		{Name: "app-one", Namespace: "user-ns"},
		{Name: "app-two", Namespace: "other-ns"},
	}

	input, calls := newInput(t, bindings)

	if err := removeRoleBindings(context.Background(), input); err != nil {
		t.Fatalf("removeRoleBindings returned an error: %v", err)
	}

	want := []deleteCall{
		{roleBindingAPIVersion, roleBindingKind, "user-ns", "app-one"},
		{roleBindingAPIVersion, roleBindingKind, "other-ns", "app-two"},
	}
	if !reflect.DeepEqual(*calls, want) {
		t.Fatalf("deleted objects:\n got %+v\nwant %+v", *calls, want)
	}
}

func TestRemoveRoleBindingsWaitsWhileTheModuleNamespaceStillRunsWorkloads(t *testing.T) {
	pod := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "Pod",
		"metadata":   map[string]any{"name": "operator-helm-controller", "namespace": settings.ModuleNamespace},
	}}

	input, calls := newInput(t, []resource{{Name: "app-one", Namespace: "user-ns"}}, pod)

	if err := removeRoleBindings(context.Background(), input); err == nil {
		t.Fatal("removeRoleBindings must fail while the namespace still has workloads")
	}

	if len(*calls) != 0 {
		t.Fatalf("no object may be deleted while workloads are running, deleted %+v", *calls)
	}
}
