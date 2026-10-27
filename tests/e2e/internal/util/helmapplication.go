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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/deckhouse/operator-helm/api/naming"
	apiv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
	"github.com/deckhouse/operator-helm/tests/e2e/internal/framework"
)

func helmApplicationInternalReleaseName(namespace, name string) string {
	return naming.DerivedName(naming.ApplicationPrefix, apiv1alpha1.HelmApplicationKind, namespace, name)
}

func HelmApplicationInternalReleaseSpec(namespace, name string) (serviceAccountName, storageNamespace string, err error) {
	release, err := framework.GetClients().DynamicClient().Resource(operatorHelmInternalHelmReleaseGVR).
		Namespace(moduleNamespace).Get(context.Background(), helmApplicationInternalReleaseName(namespace, name), metav1.GetOptions{})
	if err != nil {
		return "", "", err
	}

	serviceAccountName, _, err = unstructured.NestedString(release.Object, "spec", "serviceAccountName")
	if err != nil {
		return "", "", err
	}

	storageNamespace, _, err = unstructured.NestedString(release.Object, "spec", "storageNamespace")
	if err != nil {
		return "", "", err
	}

	return serviceAccountName, storageNamespace, nil
}

func DeleteHelmApplication(f *framework.Framework, namespace, name string, timeout time.Duration) {
	GinkgoHelper()

	err := f.OperatorClient().HelmV1alpha1().HelmApplications(namespace).
		Delete(context.Background(), name, metav1.DeleteOptions{})
	Expect(client.IgnoreNotFound(err)).NotTo(HaveOccurred(), "failed to remove HelmApplication")

	UntilHelmApplicationDeleted(namespace, name, timeout)
}

func UntilHelmApplicationDeleted(namespace, name string, timeout time.Duration) {
	GinkgoHelper()

	internalName := helmApplicationInternalReleaseName(namespace, name)

	Eventually(func(g Gomega) {
		_, err := framework.GetClients().OperatorClient().HelmV1alpha1().HelmApplications(namespace).
			Get(context.Background(), name, metav1.GetOptions{})
		g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "HelmApplication %s/%s still exists", namespace, name)

		_, err = framework.GetClients().DynamicClient().
			Resource(operatorHelmInternalHelmReleaseGVR).Namespace(moduleNamespace).
			Get(context.Background(), internalName, metav1.GetOptions{})
		g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "internal helm release %s still exists", internalName)
	}).WithTimeout(timeout).WithPolling(framework.PollingInterval).Should(Succeed())
}

func DeleteHelmApplicationRepository(f *framework.Framework, namespace, name string, timeout time.Duration) {
	GinkgoHelper()

	err := f.OperatorClient().HelmV1alpha1().HelmApplicationRepositories(namespace).
		Delete(context.Background(), name, metav1.DeleteOptions{})
	Expect(client.IgnoreNotFound(err)).NotTo(HaveOccurred(), "failed to remove HelmApplicationRepository")

	UntilHelmApplicationRepositoryDeleted(namespace, name, timeout)
}

func UntilHelmApplicationRepositoryDeleted(namespace, name string, timeout time.Duration) {
	GinkgoHelper()

	internalName := naming.DerivedName(naming.ApplicationRepositoryPrefix, apiv1alpha1.HelmApplicationRepositoryKind, namespace, name)

	Eventually(func(g Gomega) {
		_, err := framework.GetClients().OperatorClient().HelmV1alpha1().HelmApplicationRepositories(namespace).
			Get(context.Background(), name, metav1.GetOptions{})
		g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "HelmApplicationRepository %s/%s still exists", namespace, name)

		_, err = GetHelmApplicationRepositoryInternalHelmRepository(internalName)
		g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "internal helm repository %s still exists", internalName)
	}).WithTimeout(timeout).WithPolling(framework.PollingInterval).Should(Succeed())
}

func GetHelmApplicationRepositoryInternalHelmRepository(internalName string) (*unstructured.Unstructured, error) {
	return framework.GetClients().DynamicClient().Resource(operatorHelmInternalHelmRepositoryGVR).
		Namespace(moduleNamespace).Get(context.Background(), internalName, metav1.GetOptions{})
}
