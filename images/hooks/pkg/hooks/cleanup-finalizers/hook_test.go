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
	"github.com/deckhouse/module-sdk/pkg/jq"
	"github.com/deckhouse/module-sdk/testing/mock"

	"hooks/pkg/settings"
)

// patchCall records one collected patch so a test can compare the whole set at once.
type patchCall struct {
	filter     string
	apiVersion string
	kind       string
	namespace  string
	name       string
}

// newInput wires a hook input whose snapshots hold the given resources keyed by snapshot
// name and whose namespace looks free of workloads. Only PatchWithJQ is mocked, so a
// merge or JSON patch would hit an unmocked method and fail the test; the patch type is
// not an implementation detail here, see
// TestCleanupFinalizersRemovesOnlyTheModuleFinalizerAndLeavesForeignOnesInPlace.
func newInput(t *testing.T, snapshots map[string][]resource, workloads ...*unstructured.Unstructured) (*pkg.HookInput, *[]patchCall) {
	t.Helper()

	calls := make([]patchCall, 0)

	patchCollector := mock.NewPatchCollectorMock(t)
	patchCollector.PatchWithJQMock.Optional().Set(
		func(filter, apiVersion, kind, namespace, name string, _ ...pkg.PatchCollectorOption) {
			calls = append(calls, patchCall{filter, apiVersion, kind, namespace, name})
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

// applyFilter runs a collected jq filter over an object the way shell-operator does
// when it executes the patch: the object as stored is fed to the same gojq engine and
// the result is what would be written back.
func applyFilter(t *testing.T, filter string, object map[string]any) map[string]any {
	t.Helper()

	query, err := jq.NewQuery(filter)
	if err != nil {
		t.Fatalf("filter %s does not compile: %v", filter, err)
	}

	result, err := query.FilterObject(context.Background(), object)
	if err != nil {
		t.Fatalf("filter %s failed on %v: %v", filter, object, err)
	}

	filtered := map[string]any{}
	if err := json.Unmarshal([]byte(result.String()), &filtered); err != nil {
		t.Fatalf("filter %s produced %s, which is not an object: %v", filter, result, err)
	}

	return filtered
}

// objectWithFinalizers is the stored object a collected filter is replayed against.
// objectWithFinalizers builds the stored object carrying exactly the given list; with
// no arguments the list is present and empty, which is a different shape from the list
// being absent and is asserted separately.
func objectWithFinalizers(finalizers ...string) map[string]any {
	list := make([]any, 0, len(finalizers))
	for _, finalizer := range finalizers {
		list = append(list, finalizer)
	}

	return map[string]any{"metadata": map[string]any{"name": "object", "finalizers": list}}
}

// finalizersOf reads the member back, telling an empty list from an absent one.
func finalizersOf(object map[string]any) any {
	metadata, ok := object["metadata"].(map[string]any)
	if !ok {
		return nil
	}

	finalizers, present := metadata["finalizers"]
	if !present {
		return "absent"
	}

	return finalizers
}

func TestCleanupFinalizersStripsFinalizersFromInternalAndPublicKindsOfBothFamilies(t *testing.T) {
	snapshots := map[string][]resource{}
	want := make([]patchCall, 0, len(watchedKinds))

	for _, watched := range watchedKinds {
		namespace := settings.ModuleNamespace
		if !watched.moduleNamespaceOnly {
			// Whatever the binding is not narrowed to the module namespace is patched
			// wherever it was found. An empty namespace is what the snapshot carries for
			// the cluster-scoped kinds; the namespaced public ones are covered by
			// TestCleanupFinalizersPatchesNamespacedPublicObjectsInTheirOwnNamespace.
			namespace = ""
		}

		res := resource{
			APIVersion: watched.apiVersion,
			Kind:       watched.kind,
			Name:       "object",
			Namespace:  namespace,
		}
		snapshots[watched.snapshot] = []resource{res}
		want = append(want, patchCall{
			removeFinalizerFilter(watched.finalizer),
			res.APIVersion, res.Kind, res.Namespace, res.Name,
		})
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

	// Which finalizer belongs to which kind, spelled out rather than read back from the
	// table under test: the hook now removes one name instead of the whole list, so a
	// kind paired with the wrong name would leave its object finalized forever, and a
	// test deriving the expectation from the same field could not see it. The public
	// kinds carry the finalizer our controllers add; the internal flux objects carry
	// flux's own, under the name the kube-api-rewriter stores it with.
	wantFinalizers := map[string]string{
		"HelmApplication":                    "helm.deckhouse.io/cleanup",
		"HelmApplicationRepository":          "helm.deckhouse.io/cleanup",
		"HelmClusterAddon":                   "helm.deckhouse.io/cleanup",
		"HelmClusterAddonRepository":         "helm.deckhouse.io/cleanup",
		"HelmClusterApplicationRepository":   "helm.deckhouse.io/cleanup",
		"InternalNelmOperatorHelmRelease":    "finalizers.internal.operator-helm.deckhouse.io",
		"InternalNelmOperatorHelmChart":      "finalizers.internal.operator-helm.deckhouse.io",
		"InternalNelmOperatorHelmRepository": "finalizers.internal.operator-helm.deckhouse.io",
		"InternalNelmOperatorOCIRepository":  "finalizers.internal.operator-helm.deckhouse.io",
	}

	gotFinalizers := map[string]string{}
	for _, watched := range watchedKinds {
		gotFinalizers[watched.kind] = watched.finalizer
	}

	if !reflect.DeepEqual(gotFinalizers, wantFinalizers) {
		t.Errorf("finalizer per kind:\n got %v\nwant %v", gotFinalizers, wantFinalizers)
	}

	got := map[string]string{}
	for _, config := range kubernetesConfigs() {
		got[config.Kind] = config.APIVersion

		// The public kinds are not confined to the module namespace: the cluster-scoped
		// ones have no namespace at all, and HelmApplication and HelmApplicationRepository
		// are namespaced but live in arbitrary user namespaces. Neither can be found by a
		// binding narrowed to the module namespace.
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

	want := []patchCall{{removeFinalizerFilter(publicFinalizer), publicAPIVersion, "HelmApplication", "user-ns", "app"}}
	if !reflect.DeepEqual(*calls, want) {
		t.Fatalf("patched objects:\n got %+v\nwant %+v", *calls, want)
	}
}

// TestCleanupFinalizersRemovesOnlyTheModuleFinalizerAndLeavesForeignOnesInPlace replays
// the collected filter against the object it would be applied to. The collector hands
// patches to the API server only after the hook returns, so nothing here observes the
// write itself; what the filter does to the stored object is the behaviour that matters.
// A HelmApplication lives in a user namespace where a backup or policy operator may hold
// its own finalizer, and the module being torn down says nothing about whether that
// operator has finished.
func TestCleanupFinalizersRemovesOnlyTheModuleFinalizerAndLeavesForeignOnesInPlace(t *testing.T) {
	tests := []struct {
		name   string
		stored map[string]any
		want   any
	}{
		{
			name:   "a foreign finalizer next to ours survives",
			stored: objectWithFinalizers("backup.example.com/protect", publicFinalizer, "policy.example.com/guard"),
			want:   []any{"backup.example.com/protect", "policy.example.com/guard"},
		},
		{
			name:   "ours alone leaves an empty list",
			stored: objectWithFinalizers(publicFinalizer),
			want:   []any{},
		},
		{
			name:   "an object with no finalizers at all is left untouched",
			stored: map[string]any{"metadata": map[string]any{"name": "object"}},
			want:   "absent",
		},
		{
			name:   "a second run over an already stripped object changes nothing",
			stored: objectWithFinalizers("backup.example.com/protect"),
			want:   []any{"backup.example.com/protect"},
		},
		{
			// A list emptied by an earlier removal, ours or somebody else's. The guard
			// has to treat it as present, not as absent: jq's `if` is false only on
			// null and false, and an empty array is neither.
			name:   "an empty list stays an empty list",
			stored: objectWithFinalizers(),
			want:   []any{},
		},
		{
			// An explicit null is what a client that removed the last finalizer with a
			// merge patch leaves behind, and it is the shape the previous JSON Patch
			// implementation of this hook could not handle at all.
			name:   "an explicit null is left alone",
			stored: map[string]any{"metadata": map[string]any{"name": "object", "finalizers": nil}},
			want:   nil,
		},
		{
			// The API server does not create duplicates, but an object edited by two
			// clients racing on the list can carry one. Removing only the first would
			// leave the object finalized forever with nobody left to finish the job.
			name:   "every copy of ours is removed, not just the first",
			stored: objectWithFinalizers(publicFinalizer, "backup.example.com/protect", publicFinalizer),
			want:   []any{"backup.example.com/protect"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			snapshots := map[string][]resource{
				"helmApplicationSnapshot": {
					{APIVersion: publicAPIVersion, Kind: "HelmApplication", Name: "object", Namespace: "user-ns"},
				},
			}

			input, calls := newInput(t, snapshots)

			if err := cleanupFinalizer(context.Background(), input); err != nil {
				t.Fatalf("cleanupFinalizer returned an error: %v", err)
			}

			if len(*calls) != 1 {
				t.Fatalf("expected one patch, got %+v", *calls)
			}

			got := finalizersOf(applyFilter(t, (*calls)[0].filter, test.stored))
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("finalizers after the patch:\n got %#v\nwant %#v", got, test.want)
			}
		})
	}
}

// TestCleanupFinalizersStripsTheRewrittenFluxFinalizerFromInternalObjects guards the one
// string the hook cannot borrow from the api module: the kube-api-rewriter stores flux's
// finalizers.fluxcd.io under the internal domain, and the hook bypasses the rewriter.
func TestCleanupFinalizersStripsTheRewrittenFluxFinalizerFromInternalObjects(t *testing.T) {
	snapshots := map[string][]resource{
		"internalHelmReleaseSnapshot": {{
			APIVersion: "helm.internal.operator-helm.deckhouse.io/v2",
			Kind:       "InternalNelmOperatorHelmRelease",
			Name:       "object",
			Namespace:  settings.ModuleNamespace,
		}},
	}

	input, calls := newInput(t, snapshots)

	if err := cleanupFinalizer(context.Background(), input); err != nil {
		t.Fatalf("cleanupFinalizer returned an error: %v", err)
	}

	if len(*calls) != 1 {
		t.Fatalf("expected one patch, got %+v", *calls)
	}

	stored := objectWithFinalizers("finalizers.internal.operator-helm.deckhouse.io", "backup.example.com/protect")
	got := finalizersOf(applyFilter(t, (*calls)[0].filter, stored))
	if want := []any{"backup.example.com/protect"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("finalizers after the patch:\n got %#v\nwant %#v", got, want)
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
