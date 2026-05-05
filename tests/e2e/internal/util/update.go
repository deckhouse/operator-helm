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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
	"github.com/deckhouse/operator-helm/tests/e2e/internal/framework"
)

// UpdateHelmClusterAddon performs a read-modify-write cycle on a HelmClusterAddon
// with automatic retry on conflict.
func UpdateHelmClusterAddon(name string, mutate func(*apiv1alpha1.HelmClusterAddon)) *apiv1alpha1.HelmClusterAddon {
	GinkgoHelper()

	var updated *apiv1alpha1.HelmClusterAddon
	Eventually(func(g Gomega) {
		current, err := framework.GetClients().OperatorClient().HelmV1alpha1().
			HelmClusterAddons().
			Get(context.Background(), name, metav1.GetOptions{})
		g.Expect(err).NotTo(HaveOccurred())

		mutate(current)

		updated, err = framework.GetClients().OperatorClient().HelmV1alpha1().
			HelmClusterAddons().
			Update(context.Background(), current, metav1.UpdateOptions{})
		g.Expect(err).NotTo(HaveOccurred())
	}).WithTimeout(framework.LongTimeout).WithPolling(framework.PollingInterval).Should(Succeed())

	return updated
}

// UpdateHelmClusterAddonRepository performs a read-modify-write cycle on a HelmClusterAddonRepository
// with automatic retry on conflict.
func UpdateHelmClusterAddonRepository(name string, mutate func(*apiv1alpha1.HelmClusterAddonRepository)) *apiv1alpha1.HelmClusterAddonRepository {
	GinkgoHelper()

	var updated *apiv1alpha1.HelmClusterAddonRepository
	Eventually(func(g Gomega) {
		current, err := framework.GetClients().OperatorClient().HelmV1alpha1().
			HelmClusterAddonRepositories().
			Get(context.Background(), name, metav1.GetOptions{})
		g.Expect(err).NotTo(HaveOccurred())

		mutate(current)

		updated, err = framework.GetClients().OperatorClient().HelmV1alpha1().
			HelmClusterAddonRepositories().
			Update(context.Background(), current, metav1.UpdateOptions{})
		g.Expect(err).NotTo(HaveOccurred())
	}).WithTimeout(framework.LongTimeout).WithPolling(framework.PollingInterval).Should(Succeed())

	return updated
}
