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

package framework

import (
	"context"
	"fmt"
	"slices"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	NamespacePrefix = "e2e"
	E2ELabel        = "e2e-test"
)

type Framework struct {
	Clients

	namespacePrefix  string
	namespace        *corev1.Namespace
	objectsToDelete  []client.Object
	trackedForDelete map[string]struct{}
	deferredDeletes  []func() error
}

func NewFramework(prefix string) *Framework {
	return &Framework{
		Clients:         GetClients(),
		namespacePrefix: prefix,
	}
}

// Before creates an isolated namespace for the test.
// Pass empty prefix to NewFramework to skip namespace creation.
func (f *Framework) Before() {
	GinkgoHelper()

	if f.namespacePrefix == "" {
		return
	}

	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: fmt.Sprintf("%s-%s-", NamespacePrefix, f.namespacePrefix),
			Labels:       map[string]string{E2ELabel: "true"},
		},
	}

	err := f.generic.Create(context.Background(), ns)
	Expect(err).NotTo(HaveOccurred())
	By(fmt.Sprintf("Namespace %q has been created", ns.Name))
	f.namespace = ns
}

// After handles cleanup and dump on failure.
func (f *Framework) After() {
	GinkgoHelper()

	if CurrentSpecReport().Failed() {
		f.saveDump()
	}

	if !IsCleanUpNeeded() {
		return
	}

	for _, fn := range f.deferredDeletes {
		_ = fn()
	}

	slices.Reverse(f.objectsToDelete)

	for _, obj := range f.objectsToDelete {
		_ = f.generic.Delete(context.Background(), obj)
	}
	f.waitDeleted(f.objectsToDelete)
}

func (f *Framework) Namespace() *corev1.Namespace {
	return f.namespace
}

func (f *Framework) NamespaceName() string {
	if f.namespace == nil {
		return ""
	}
	return f.namespace.Name
}

// Create creates resources via the generic client and registers them for cleanup.
func (f *Framework) Create(ctx context.Context, objs ...client.Object) error {
	for _, obj := range objs {
		labels := obj.GetLabels()
		if labels == nil {
			labels = make(map[string]string)
		}
		obj.SetLabels(labels)

		if err := f.generic.Create(ctx, obj); err != nil {
			return err
		}
		f.appendForDelete(obj)
	}
	return nil
}

// EnsureCreate gets the object by key; if it exists, registers it for cleanup only.
// If it is not found, creates it via Create (which registers for cleanup).
func (f *Framework) EnsureCreate(ctx context.Context, obj client.Object) error {
	GinkgoHelper()
	key := client.ObjectKeyFromObject(obj)
	clone := obj.DeepCopyObject().(client.Object)
	err := f.generic.Get(ctx, key, clone)
	if err == nil {
		f.appendForDelete(clone)
		return nil
	}
	if !k8serrors.IsNotFound(err) {
		return err
	}
	return f.Create(ctx, obj)
}

// EnsureDynamic creates or updates an unstructured resource via the dynamic client,
// then registers it for cleanup (once per resource key).
func (f *Framework) EnsureDynamic(ctx context.Context, gvr schema.GroupVersionResource, namespace string, desired *unstructured.Unstructured, clusterScoped bool) error {
	return f.ensureDynamic(ctx, gvr, namespace, desired, clusterScoped, true)
}

// EnsureDynamicWithoutCleanup is like EnsureDynamic but does not register the object for Framework.After().
func (f *Framework) EnsureDynamicWithoutCleanup(ctx context.Context, gvr schema.GroupVersionResource, namespace string, desired *unstructured.Unstructured, clusterScoped bool) error {
	return f.ensureDynamic(ctx, gvr, namespace, desired, clusterScoped, false)
}

func (f *Framework) ensureDynamic(ctx context.Context, gvr schema.GroupVersionResource, namespace string, desired *unstructured.Unstructured, clusterScoped, registerForCleanup bool) error {
	name := desired.GetName()
	var ri dynamic.ResourceInterface
	if clusterScoped {
		ri = f.DynamicClient().Resource(gvr)
	} else {
		ri = f.DynamicClient().Resource(gvr).Namespace(namespace)
	}

	existing, err := ri.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if !k8serrors.IsNotFound(err) {
			return fmt.Errorf("get %s %q: %w", gvr.Resource, name, err)
		}
		_, err = ri.Create(ctx, desired, metav1.CreateOptions{})
		if err != nil {
			return fmt.Errorf("create %s %q: %w", gvr.Resource, name, err)
		}
		if registerForCleanup {
			f.appendForDelete(desired)
		}
		return nil
	}

	desired.SetResourceVersion(existing.GetResourceVersion())
	_, err = ri.Update(ctx, desired, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("update %s %q: %w", gvr.Resource, name, err)
	}
	if registerForCleanup {
		f.appendForDelete(desired)
	}
	return nil
}

func (f *Framework) appendForDelete(obj client.Object) {
	if f.trackedForDelete == nil {
		f.trackedForDelete = make(map[string]struct{})
	}
	key := f.objectDeleteCacheKey(obj)
	if _, ok := f.trackedForDelete[key]; ok {
		return
	}
	f.trackedForDelete[key] = struct{}{}
	f.objectsToDelete = append(f.objectsToDelete, obj)
}

func (f *Framework) objectDeleteCacheKey(obj client.Object) string {
	gvk := obj.GetObjectKind().GroupVersionKind()
	return fmt.Sprintf("%s/%s/%s/%s/%s", gvk.Group, gvk.Version, gvk.Kind, obj.GetNamespace(), obj.GetName())
}

// DeferDelete registers objects for cleanup in After().
func (f *Framework) DeferDelete(objs ...client.Object) {
	for _, obj := range objs {
		f.appendForDelete(obj)
	}
}

// DeferDeleteFunc registers a custom cleanup function.
func (f *Framework) DeferDeleteFunc(fn func() error) {
	f.deferredDeletes = append(f.deferredDeletes, fn)
}

func (f *Framework) waitDeleted(objs []client.Object) {
	for _, obj := range objs {
		key := types.NamespacedName{
			Namespace: obj.GetNamespace(),
			Name:      obj.GetName(),
		}
		_ = wait.PollUntilContextTimeout(
			context.Background(), time.Second, LongTimeout, true,
			func(ctx context.Context) (bool, error) {
				err := f.generic.Get(ctx, key, obj)
				if k8serrors.IsNotFound(err) {
					return true, nil
				}
				return false, nil
			},
		)
	}
}
