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

package helmclusteraddon

import (
	"context"
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
	"github.com/deckhouse/operator-helm/tests/e2e/internal/controller"
	"github.com/deckhouse/operator-helm/tests/e2e/internal/framework"
	"github.com/deckhouse/operator-helm/tests/e2e/internal/util"
)

// claimLeaseNames returns the names of the claim Leases the controller currently
// records for an addon. Each claim Lease carries the source-name label, so the
// leases an addon owns can be listed without recomputing the hashed Lease name.
func claimLeaseNames(addonName string) []string {
	GinkgoHelper()

	selector := fmt.Sprintf("%s=%s,%s=%s",
		apiv1alpha1.LabelManagedBy, apiv1alpha1.LabelManagedByValue,
		apiv1alpha1.HelmClusterAddonLabelSourceName, addonName,
	)

	list, err := framework.GetClients().KubeClient().CoordinationV1().
		Leases(apiv1alpha1.TargetNamespace).
		List(context.Background(), metav1.ListOptions{LabelSelector: selector})
	Expect(err).NotTo(HaveOccurred())

	names := make([]string, 0, len(list.Items))
	for _, lease := range list.Items {
		names = append(names, lease.Name)
	}
	return names
}

var _ = Describe("HelmClusterAddon chart claim", Ordered, func() {
	f := framework.NewFramework("addon-chart-claim")

	const (
		repoURL   = "https://stefanprodan.github.io/podinfo"
		chartName = "podinfo"
		version   = "6.10.2"
	)

	repoAName := "e2e-claim-repo-a"
	repoBName := "e2e-claim-repo-b"
	addonAName := "e2e-claim-addon-a"
	addonBName := "e2e-claim-addon-b"

	// A dedicated namespace for addon B: it installs the same chart as addon A, so
	// it must land in a separate namespace to avoid colliding on release resources.
	var addonBNamespace string

	// The name of the claim Lease addon A holds while it points at repoA; captured
	// before the chart reference change so its release can be asserted afterwards.
	var repoAClaimLease string

	BeforeAll(func() {
		DeferCleanup(f.After)
		f.Before()

		nsB := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: fmt.Sprintf("%s-addon-chart-claim-b-", framework.NamespacePrefix),
				Labels:       map[string]string{framework.E2ELabel: "true"},
			},
		}
		Expect(f.GenericClient().Create(context.Background(), nsB)).To(Succeed())
		f.DeferDelete(nsB)
		addonBNamespace = nsB.Name
	})

	AfterEach(func() {
		By("Verifying no errors in operator-helm-controller logs")
		controller.AssertNoErrorsFor("operator-helm-controller")
	})

	It("should create two repositories pointing at the same source", func() {
		for _, name := range []string{repoAName, repoBName} {
			repo := &apiv1alpha1.HelmClusterAddonRepository{
				ObjectMeta: metav1.ObjectMeta{Name: name},
				Spec: apiv1alpha1.HelmClusterAddonRepositorySpec{
					URL:                repoURL,
					InsecureSkipVerify: false,
				},
			}

			created, err := f.OperatorClient().HelmV1alpha1().
				HelmClusterAddonRepositories().
				Create(context.Background(), repo, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			f.DeferDelete(created)

			By(fmt.Sprintf("Waiting for repository %q to become Ready and Synced", name))
			util.UntilConditionTrue(apiv1alpha1.ConditionTypeReady, framework.LongTimeout, created)
			util.UntilConditionTrue(apiv1alpha1.ConditionTypeSynced, framework.LongTimeout, created)
		}
	})

	It("should create addon A and claim the repository/chart pair", func() {
		addon := &apiv1alpha1.HelmClusterAddon{
			ObjectMeta: metav1.ObjectMeta{Name: addonAName},
			Spec: apiv1alpha1.HelmClusterAddonSpec{
				Chart: apiv1alpha1.HelmClusterAddonChartRef{
					HelmClusterAddonChartName:  chartName,
					HelmClusterAddonRepository: repoAName,
					Version:                    version,
				},
				Namespace: f.NamespaceName(),
			},
		}

		created, err := f.OperatorClient().HelmV1alpha1().
			HelmClusterAddons().
			Create(context.Background(), addon, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		f.DeferDelete(created)

		By("Waiting for addon A to become Ready")
		util.UntilConditionTrue(apiv1alpha1.ConditionTypeReady, framework.LongTimeout, created)

		By("Verifying addon A holds exactly one claim Lease")
		Eventually(func(g Gomega) {
			leases := claimLeaseNames(addonAName)
			g.Expect(leases).To(HaveLen(1))
			repoAClaimLease = leases[0]
		}).WithTimeout(framework.LongTimeout).WithPolling(framework.PollingInterval).Should(Succeed())
	})

	It("should reject a second addon that reuses the same repository and chart", func() {
		duplicate := &apiv1alpha1.HelmClusterAddon{
			ObjectMeta: metav1.ObjectMeta{Name: addonBName},
			Spec: apiv1alpha1.HelmClusterAddonSpec{
				Chart: apiv1alpha1.HelmClusterAddonChartRef{
					HelmClusterAddonChartName:  chartName,
					HelmClusterAddonRepository: repoAName,
					Version:                    version,
				},
				Namespace: addonBNamespace,
			},
		}

		By("Attempting to create a duplicate addon")
		_, err := f.OperatorClient().HelmV1alpha1().
			HelmClusterAddons().
			Create(context.Background(), duplicate, metav1.CreateOptions{})
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("already used by helmclusteraddon/" + addonAName))

		By("Verifying addon A still holds a single claim Lease")
		Expect(claimLeaseNames(addonAName)).To(ConsistOf(repoAClaimLease))
	})

	It("should release the stale claim when addon A changes its repository", func() {
		By("Repointing addon A to the second repository")
		updated := util.UpdateHelmClusterAddon(addonAName, func(addon *apiv1alpha1.HelmClusterAddon) {
			addon.Spec.Chart.HelmClusterAddonRepository = repoBName
		})

		By("Waiting for addon A to reconcile against the new repository")
		util.UntilConditionTrue(apiv1alpha1.ConditionTypeReady, framework.LongTimeout, updated)

		By("Verifying the old claim Lease is released and a single new one is held")
		Eventually(func(g Gomega) {
			leases := claimLeaseNames(addonAName)
			g.Expect(leases).To(HaveLen(1))
			g.Expect(leases).NotTo(ContainElement(repoAClaimLease))
		}).WithTimeout(framework.LongTimeout).WithPolling(framework.PollingInterval).Should(Succeed())

		By("Verifying the old Lease object no longer exists")
		Eventually(func(g Gomega) {
			_, err := f.KubeClient().CoordinationV1().
				Leases(apiv1alpha1.TargetNamespace).
				Get(context.Background(), repoAClaimLease, metav1.GetOptions{})
			g.Expect(err).To(HaveOccurred())
		}).WithTimeout(framework.LongTimeout).WithPolling(framework.PollingInterval).Should(Succeed())
	})

	It("should let a new addon claim the released repository/chart pair", func() {
		reuse := &apiv1alpha1.HelmClusterAddon{
			ObjectMeta: metav1.ObjectMeta{Name: addonBName},
			Spec: apiv1alpha1.HelmClusterAddonSpec{
				Chart: apiv1alpha1.HelmClusterAddonChartRef{
					HelmClusterAddonChartName:  chartName,
					HelmClusterAddonRepository: repoAName,
					Version:                    version,
				},
				Namespace: addonBNamespace,
			},
		}

		By("Creating addon B on the freed repository/chart pair")
		created, err := f.OperatorClient().HelmV1alpha1().
			HelmClusterAddons().
			Create(context.Background(), reuse, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		f.DeferDelete(created)

		By("Waiting for addon B to become Ready")
		util.UntilConditionTrue(apiv1alpha1.ConditionTypeReady, framework.LongTimeout, created)

		By("Verifying addon B did not surface a chart claim conflict")
		claimLease := claimLeaseNames(addonBName)
		Expect(claimLease).To(HaveLen(1))
		Expect(claimLease).To(ConsistOf(repoAClaimLease))
	})
})
