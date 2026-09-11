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
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/deckhouse/operator-helm/tests/e2e/internal/framework"
	"github.com/deckhouse/operator-helm/tests/e2e/internal/naming"
)

// ApplicationServiceAccountName reproduces the name operator-helm-controller
// derives for an application's internal objects — the ServiceAccount it is applied
// as, its RoleBinding, its HelmRelease. The tests derive it rather than
// hard-coding one so a rename of an application under test does not silently stop
// checking anything. See internal/naming for the derivation itself.
func ApplicationServiceAccountName(namespace, name string) string {
	return naming.ApplicationServiceAccountName(namespace, name)
}

// DeleteHelmApplication removes the application and waits until its internal helm
// release is gone, which is what says the uninstall actually finished.
func DeleteHelmApplication(f *framework.Framework, namespace, name string, timeout time.Duration) {
	GinkgoHelper()

	err := f.OperatorClient().HelmV1alpha1().HelmApplications(namespace).
		Delete(context.Background(), name, metav1.DeleteOptions{})
	Expect(client.IgnoreNotFound(err)).NotTo(HaveOccurred(), "failed to remove HelmApplication")

	UntilHelmApplicationDeleted(namespace, name, timeout)
}

// UntilHelmApplicationDeleted waits until the application object is gone.
func UntilHelmApplicationDeleted(namespace, name string, timeout time.Duration) {
	GinkgoHelper()

	Eventually(func(g Gomega) {
		_, err := framework.GetClients().OperatorClient().HelmV1alpha1().HelmApplications(namespace).
			Get(context.Background(), name, metav1.GetOptions{})
		g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "HelmApplication %s/%s still exists", namespace, name)
	}).WithTimeout(timeout).WithPolling(framework.PollingInterval).Should(Succeed())
}

// DeleteHelmApplicationRepository removes the repository and waits until it is gone.
func DeleteHelmApplicationRepository(f *framework.Framework, namespace, name string, timeout time.Duration) {
	GinkgoHelper()

	err := f.OperatorClient().HelmV1alpha1().HelmApplicationRepositories(namespace).
		Delete(context.Background(), name, metav1.DeleteOptions{})
	Expect(client.IgnoreNotFound(err)).NotTo(HaveOccurred(), "failed to remove HelmApplicationRepository")

	Eventually(func(g Gomega) {
		_, err := framework.GetClients().OperatorClient().HelmV1alpha1().HelmApplicationRepositories(namespace).
			Get(context.Background(), name, metav1.GetOptions{})
		g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "HelmApplicationRepository %s/%s still exists", namespace, name)
	}).WithTimeout(timeout).WithPolling(framework.PollingInterval).Should(Succeed())
}
