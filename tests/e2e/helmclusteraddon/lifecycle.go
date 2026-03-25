package helmclusteraddon

import (
	"context"
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
	"github.com/deckhouse/operator-helm/tests/e2e/internal/controller"
	"github.com/deckhouse/operator-helm/tests/e2e/internal/framework"
	"github.com/deckhouse/operator-helm/tests/e2e/internal/util"
)

var _ = Describe("HelmClusterAddon lifecycle", Ordered, func() {
	f := framework.NewFramework("addon-lifecycle")
	cfg := framework.GetConfig()

	const (
		repoName  = "e2e-test-repo"
		addonName = "e2e-test-addon"
		chartName = "podinfo"
	)

	BeforeAll(func() {
		DeferCleanup(f.After)
		f.Before()

		By("Verifying all controllers are running")
		for _, ctrl := range cfg.Controllers {
			util.UntilControllerReady(ctrl.Namespace, ctrl.LabelSelector, framework.LongTimeout)
		}
	})

	It("should create HelmClusterAddonRepository and reach Ready", func() {
		repo := &apiv1alpha1.HelmClusterAddonRepository{
			ObjectMeta: metav1.ObjectMeta{
				Name: repoName,
			},
			Spec: apiv1alpha1.HelmClusterAddonRepositorySpec{
				URL:       "https://stefanprodan.github.io/podinfo",
				TLSVerify: true,
			},
		}

		created, err := f.OperatorClient().HelmV1alpha1().
			HelmClusterAddonRepositories().
			Create(context.Background(), repo, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())

		f.DeferDeleteFunc(func() error {
			return f.OperatorClient().HelmV1alpha1().
				HelmClusterAddonRepositories().
				Delete(context.Background(), created.Name, metav1.DeleteOptions{})
		})

		By("Waiting for repository to become Ready")
		util.UntilConditionTrue(
			apiv1alpha1.ConditionTypeReady,
			framework.LongTimeout,
			created,
		)

		By("Verifying no errors in controllers after repo creation")
		controller.AssertNoErrorsFor("operator-helm-controller")
	})

	It("should verify target namespace does not have addon pods yet", func() {
		By(fmt.Sprintf("Checking namespace %q has no addon pods", f.NamespaceName()))
		pods, err := f.KubeClient().CoreV1().
			Pods(f.NamespaceName()).
			List(context.Background(), metav1.ListOptions{
				LabelSelector: fmt.Sprintf("app.kubernetes.io/name=%s-%s", addonName, chartName),
			})
		Expect(err).NotTo(HaveOccurred())
		Expect(pods.Items).To(BeEmpty())
	})

	It("should create HelmClusterAddon and wait for installation", func() {
		addon := &apiv1alpha1.HelmClusterAddon{
			ObjectMeta: metav1.ObjectMeta{
				Name: addonName,
			},
			Spec: apiv1alpha1.HelmClusterAddonSpec{
				Chart: apiv1alpha1.HelmClusterAddonChartRef{
					HelmClusterAddonChartName:  chartName,
					HelmClusterAddonRepository: repoName,
					Version:                    "6.10.2",
				},
				Namespace: f.NamespaceName(),
			},
		}

		created, err := f.OperatorClient().HelmV1alpha1().
			HelmClusterAddons().
			Create(context.Background(), addon, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())

		f.DeferDeleteFunc(func() error {
			return f.OperatorClient().HelmV1alpha1().
				HelmClusterAddons().
				Delete(context.Background(), created.Name, metav1.DeleteOptions{})
		})

		By("Waiting for addon to be installed")
		util.UntilConditionTrue(
			apiv1alpha1.ConditionTypeReady,
			framework.LongTimeout,
			created,
		)

		By("Verifying Installed condition reason is Success")
		util.UntilConditionReason(
			apiv1alpha1.ConditionTypeInstalled,
			"InstallSucceeded",
			framework.ShortTimeout,
			created,
		)
	})

	It("should have pods running in the target namespace", func() {
		labelSelector := fmt.Sprintf("app.kubernetes.io/name=%s-%s", addonName, chartName)

		By("Checking all pods are ready")
		util.UntilAllPodsReady(f.NamespaceName(), labelSelector, 1, framework.LongTimeout)
	})

	It("should update chart version and apply changes", func() {
		By("Updating addon chart version")
		updated := util.UpdateHelmClusterAddon(addonName, func(addon *apiv1alpha1.HelmClusterAddon) {
			addon.Spec.Chart.Version = "6.10.0"
		})

		By("Waiting for update to be applied")
		util.UntilConditionTrue(
			apiv1alpha1.ConditionTypeReady,
			framework.LongTimeout,
			updated,
		)

		By("Verifying the version was applied")
		updated, err := f.OperatorClient().HelmV1alpha1().
			HelmClusterAddons().
			Get(context.Background(), addonName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(updated.Status.LastAppliedChart).NotTo(BeNil())
		Expect(updated.Status.LastAppliedChart.Version).To(Equal("6.10.0"))
	})

	It("should verify pod count after update", func() {
		labelSelector := fmt.Sprintf("app.kubernetes.io/name=%s-%s", addonName, chartName)

		By("Verifying pods are still running after update")
		util.UntilPodCount(f.NamespaceName(), labelSelector, 1, framework.LongTimeout)
	})

	It("should have no errors in any controller", func() {
		controller.AssertNoErrors()
	})
})
