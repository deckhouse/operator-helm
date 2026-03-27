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

package util

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/deckhouse/operator-helm/tests/e2e/internal/framework"
)

// AssertNamespaceAbsent verifies the namespace does not exist.
func AssertNamespaceAbsent(name string) {
	GinkgoHelper()
	_, err := framework.GetClients().KubeClient().CoreV1().
		Namespaces().Get(context.Background(), name, metav1.GetOptions{})
	Expect(k8serrors.IsNotFound(err)).To(BeTrue(),
		"namespace %q should not exist, but it does", name)
}

// UntilNamespaceAbsent waits for the namespace to be fully deleted.
func UntilNamespaceAbsent(name string, timeout time.Duration) {
	GinkgoHelper()
	Eventually(func(g Gomega) {
		_, err := framework.GetClients().KubeClient().CoreV1().
			Namespaces().Get(context.Background(), name, metav1.GetOptions{})
		g.Expect(k8serrors.IsNotFound(err)).To(BeTrue(),
			"namespace %q still exists", name)
	}).WithTimeout(timeout).WithPolling(time.Second).Should(Succeed())
}

// AssertNamespaceExists verifies the namespace exists.
func AssertNamespaceExists(name string) {
	GinkgoHelper()
	_, err := framework.GetClients().KubeClient().CoreV1().
		Namespaces().Get(context.Background(), name, metav1.GetOptions{})
	Expect(err).NotTo(HaveOccurred(), "namespace %q should exist", name)
}

// EnsureNamespace creates a namespace if it does not already exist.
func EnsureNamespace(name string, labels map[string]string) *corev1.Namespace {
	GinkgoHelper()

	existing, err := framework.GetClients().KubeClient().CoreV1().
		Namespaces().Get(context.Background(), name, metav1.GetOptions{})
	if err == nil {
		return existing
	}
	Expect(k8serrors.IsNotFound(err)).To(BeTrue(),
		"unexpected error checking namespace %q: %v", name, err)

	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: labels,
		},
	}
	created, err := framework.GetClients().KubeClient().CoreV1().
		Namespaces().Create(context.Background(), ns, metav1.CreateOptions{})
	Expect(err).NotTo(HaveOccurred(), "failed to create namespace %q", name)
	return created
}

// DeleteNamespace deletes a namespace and optionally waits for it to disappear.
func DeleteNamespace(name string, wait bool, timeout time.Duration) {
	GinkgoHelper()
	err := framework.GetClients().KubeClient().CoreV1().
		Namespaces().Delete(context.Background(), name, metav1.DeleteOptions{})
	if k8serrors.IsNotFound(err) {
		return
	}
	Expect(err).NotTo(HaveOccurred(), "failed to delete namespace %q", name)

	if wait {
		UntilNamespaceAbsent(name, timeout)
	}
}
