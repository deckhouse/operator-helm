package framework

import (
	"context"
	"fmt"
	"maps"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	NamespacePrefix = "e2e"
	E2ELabel        = "e2e-test"
)

type Framework struct {
	Clients

	namespacePrefix string
	namespace       *corev1.Namespace
	objectsToDelete []client.Object
	deferredDeletes []func() error
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

	for _, obj := range f.objectsToDelete {
		_ = f.generic.Delete(context.Background(), obj)
	}
	f.waitDeleted(f.objectsToDelete)

	if f.namespace != nil {
		By("Cleanup: delete namespace")
		err := f.generic.Delete(context.Background(), f.namespace)
		if err != nil && !k8serrors.IsNotFound(err) {
			Expect(err).NotTo(HaveOccurred())
		}
	}
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
		maps.Copy(labels, map[string]string{E2ELabel: f.namespacePrefix})
		obj.SetLabels(labels)

		if err := f.generic.Create(ctx, obj); err != nil {
			return err
		}
		f.objectsToDelete = append(f.objectsToDelete, obj)
	}
	return nil
}

// DeferDelete registers objects for cleanup in After().
func (f *Framework) DeferDelete(objs ...client.Object) {
	f.objectsToDelete = append(f.objectsToDelete, objs...)
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
