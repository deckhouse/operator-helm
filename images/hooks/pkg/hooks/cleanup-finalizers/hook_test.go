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

package cleanup_finalizers

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

// patchCall records one collected patch so a test can compare the whole set at once.
type patchCall struct {
	patch      any
	apiVersion string
	kind       string
	namespace  string
	name       string
}

// newInput wires a hook input whose snapshots hold the given resources keyed by snapshot
// name and whose namespace looks free of workloads. The patch collector only accepts
// merge patches: a JSON patch would hit an unmocked method and fail the test, which is
// the point — the JSON patch variant is what was not idempotent.
func newInput(t *testing.T, snapshots map[string][]resource, workloads ...*unstructured.Unstructured) (*pkg.HookInput, *[]patchCall) {
	t.Helper()

	calls := make([]patchCall, 0)

	patchCollector := mock.NewPatchCollectorMock(t)
	patchCollector.PatchWithMergeMock.Optional().Set(
		func(patch any, apiVersion, kind, namespace, name string, _ ...pkg.PatchCollectorOption) {
			calls = append(calls, patchCall{patch, apiVersion, kind, namespace, name})
		},
	)

	snapshotsMock := mock.NewSnapshotsMock(t)
	snapshotsMock.GetMock.Optional().Set(func(key string) []pkg.Snapshot {
		result := make([]pkg.Snapshot, 0, len(snapshots[key]))
		for _, res := range snapshots[key] {
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

// snapshotOf mimics what the jq filter of the binding produces for one object.
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

// k8sClient serves the dynamic client the hook probes workloads with. The embedded
// interface is nil on purpose: the hook touches nothing else, and a call to any other
// method should panic loudly rather than silently pass.
type k8sClient struct {
	pkg.KubernetesClient

	dynamic dynamic.Interface
}

func (c k8sClient) Dynamic() dynamic.Interface { return c.dynamic }

// fakeDynamicClient answers the workload probe with the supplied objects, whatever
// resource and namespace is asked for.
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

func nullFinalizersPatch() map[string]any {
	return map[string]any{"metadata": map[string]any{"finalizers": nil}}
}

func TestCleanupFinalizersStripsFinalizersFromInternalAndPublicKindsOfBothFamilies(t *testing.T) {
	snapshots := map[string][]resource{}
	want := make([]patchCall, 0, len(watchedKinds))

	for _, watched := range watchedKinds {
		namespace := settings.ModuleNamespace
		if !watched.moduleNamespaceOnly {
			namespace = "" // cluster-scoped objects carry no namespace in the snapshot
		}

		res := resource{
			APIVersion: watched.apiVersion,
			Kind:       watched.kind,
			Name:       "object",
			Namespace:  namespace,
		}
		snapshots[watched.snapshot] = []resource{res}
		want = append(want, patchCall{nullFinalizersPatch(), res.APIVersion, res.Kind, res.Namespace, res.Name})
	}

	input, calls := newInput(t, snapshots)

	if err := cleanupFinalizer(context.Background(), input); err != nil {
		t.Fatalf("cleanupFinalizer returned an error: %v", err)
	}

	if !reflect.DeepEqual(*calls, want) {
		t.Fatalf("patched objects:\n got %+v\nwant %+v", *calls, want)
	}
}

func TestCleanupFinalizersCoversEveryKindThatCarriesTheCleanupFinalizer(t *testing.T) {
	// The finalizer helm.deckhouse.io/cleanup is put on the release kinds and on all
	// three repository kinds by the controllers; after the module is deleted nobody
	// else can take it off, so each of them must be watched by the hook.
	want := map[string]string{
		"HelmApplication":                    publicAPIVersion,
		"HelmApplicationRepository":          publicAPIVersion,
		"HelmClusterAddon":                   publicAPIVersion,
		"HelmClusterAddonRepository":         publicAPIVersion,
		"HelmClusterApplicationRepository":   publicAPIVersion,
		"InternalNelmOperatorHelmRelease":    "helm.internal.operator-helm.deckhouse.io/v2",
		"InternalNelmOperatorHelmChart":      "source.internal.operator-helm.deckhouse.io/v1",
		"InternalNelmOperatorHelmRepository": "source.internal.operator-helm.deckhouse.io/v1",
		"InternalNelmOperatorOCIRepository":  "source.internal.operator-helm.deckhouse.io/v1",
	}

	got := map[string]string{}
	for _, config := range kubernetesConfigs() {
		got[config.Kind] = config.APIVersion

		// Public objects live in user namespaces, so their bindings must not be
		// narrowed to the module namespace.
		if config.APIVersion == publicAPIVersion && config.NamespaceSelector != nil {
			t.Errorf("%s is watched only in %v, but it may live in any namespace",
				config.Kind, config.NamespaceSelector.NameSelector)
		}
	}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("watched kinds:\n got %v\nwant %v", got, want)
	}
}

func TestCleanupFinalizersPatchesNamespacedPublicObjectsInTheirOwnNamespace(t *testing.T) {
	snapshots := map[string][]resource{
		"helmApplicationSnapshot": {
			{APIVersion: publicAPIVersion, Kind: "HelmApplication", Name: "app", Namespace: "user-ns"},
		},
	}

	input, calls := newInput(t, snapshots)

	if err := cleanupFinalizer(context.Background(), input); err != nil {
		t.Fatalf("cleanupFinalizer returned an error: %v", err)
	}

	want := []patchCall{{nullFinalizersPatch(), publicAPIVersion, "HelmApplication", "user-ns", "app"}}
	if !reflect.DeepEqual(*calls, want) {
		t.Fatalf("patched objects:\n got %+v\nwant %+v", *calls, want)
	}
}

func TestCleanupFinalizersRepeatsTheSamePatchOnASecondRun(t *testing.T) {
	// A teardown can run the hook more than once, and the second pass sees objects
	// whose finalizers the first pass already removed. Nulling the member with a merge
	// patch stays valid there, unlike a JSON patch removing /metadata/finalizers, which
	// the API server rejects when the path is absent.
	snapshots := map[string][]resource{
		"helmClusterAddonSnapshot": {
			{APIVersion: publicAPIVersion, Kind: "HelmClusterAddon", Name: "addon"},
		},
	}

	input, calls := newInput(t, snapshots)

	for run := 1; run <= 2; run++ {
		if err := cleanupFinalizer(context.Background(), input); err != nil {
			t.Fatalf("run %d returned an error: %v", run, err)
		}
	}

	want := []patchCall{
		{nullFinalizersPatch(), publicAPIVersion, "HelmClusterAddon", "", "addon"},
		{nullFinalizersPatch(), publicAPIVersion, "HelmClusterAddon", "", "addon"},
	}
	if !reflect.DeepEqual(*calls, want) {
		t.Fatalf("patched objects:\n got %+v\nwant %+v", *calls, want)
	}
}

func TestCleanupFinalizersWaitsWhileTheModuleNamespaceStillRunsWorkloads(t *testing.T) {
	// Stripping finalizers while a controller is alive lets it recreate them, so the
	// hook must fail and be retried instead.
	pod := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "Pod",
		"metadata":   map[string]any{"name": "operator-helm-controller", "namespace": settings.ModuleNamespace},
	}}

	snapshots := map[string][]resource{
		"helmApplicationSnapshot": {
			{APIVersion: publicAPIVersion, Kind: "HelmApplication", Name: "app", Namespace: "user-ns"},
		},
	}

	input, calls := newInput(t, snapshots, pod)

	if err := cleanupFinalizer(context.Background(), input); err == nil {
		t.Fatal("cleanupFinalizer must fail while the namespace still has workloads")
	}

	if len(*calls) != 0 {
		t.Fatalf("no object may be patched while workloads are running, patched %+v", *calls)
	}
}
