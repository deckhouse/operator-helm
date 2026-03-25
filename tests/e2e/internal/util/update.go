package util

import (
	"context"
	"time"

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
	}).WithTimeout(framework.ShortTimeout).WithPolling(time.Second).Should(Succeed())

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
	}).WithTimeout(framework.ShortTimeout).WithPolling(time.Second).Should(Succeed())

	return updated
}
