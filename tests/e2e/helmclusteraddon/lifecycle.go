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

	apiv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
	"github.com/deckhouse/operator-helm/tests/e2e/internal/controller"
	"github.com/deckhouse/operator-helm/tests/e2e/internal/framework"
	"github.com/deckhouse/operator-helm/tests/e2e/internal/util"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

		By("Waiting for HelmClusterAddonRepository to become Synced")
		util.UntilConditionTrue(
			apiv1alpha1.ConditionTypeSynced,
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

		labelSelector := fmt.Sprintf("app.kubernetes.io/name=%s-%s", addonName, chartName)

		By("Checking all pods are ready")
		util.UntilAllPodsReady(f.NamespaceName(), labelSelector, 1, framework.LongTimeout)

		By("Verifying no errors in controllers after addon creation")
		controller.AssertNoErrorsFor("operator-helm-controller")
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

		labelSelector := fmt.Sprintf("app.kubernetes.io/name=%s-%s", addonName, chartName)

		By("Verifying pods are still running after update")
		util.UntilPodCount(f.NamespaceName(), labelSelector, 1, framework.LongTimeout)

		By("Verifying no errors in controllers after addon update")
		controller.AssertNoErrorsFor("operator-helm-controller")
	})

	It("should update last applied values", func() {
		expectedValues := `{"replicaCount": 2}`

		By("Updating last applied values")
		updated := util.UpdateHelmClusterAddon(addonName, func(addon *apiv1alpha1.HelmClusterAddon) {
			addon.Spec.Values = &apiextensionsv1.JSON{Raw: []byte(expectedValues)}
		})

		By("Waiting for update to be applied")
		util.UntilConditionTrue(
			apiv1alpha1.ConditionTypeReady,
			framework.LongTimeout,
			updated,
		)

		By("Verifying that values are applied")
		updated, err := f.OperatorClient().HelmV1alpha1().
			HelmClusterAddons().
			Get(context.Background(), addonName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(updated.Status.LastAppliedValues).NotTo(BeNil())
		Expect(updated.Status.LastAppliedValues.Raw).To(MatchJSON(expectedValues))

		labelSelector := fmt.Sprintf("app.kubernetes.io/name=%s-%s", addonName, chartName)

		By("Verifying pods number changed after values update")
		util.UntilPodCount(f.NamespaceName(), labelSelector, 2, framework.LongTimeout)

		By("Verifying no errors in controllers after addon values update")
		controller.AssertNoErrorsFor("operator-helm-controller")
	})

	It("should not update chart version on invalid chart version", func() {
		addon, err := f.OperatorClient().HelmV1alpha1().
			HelmClusterAddons().
			Get(context.Background(), addonName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())

		By("Should have PartiallyDegraded condition inactive")
		util.UntilConditionStatus(
			apiv1alpha1.ConditionTypePartiallyDegraded,
			string(metav1.ConditionFalse),
			framework.LongTimeout,
			addon,
		)

		invalidChartVersion := "2000"

		By("Updating addon chart to invalid version")
		updated := util.UpdateHelmClusterAddon(addonName, func(addon *apiv1alpha1.HelmClusterAddon) {
			addon.Spec.Chart.Version = invalidChartVersion
		})

		By("Should have PartiallyDegraded condition active")
		util.UntilConditionTrue(
			apiv1alpha1.ConditionTypePartiallyDegraded,
			framework.LongTimeout,
			updated,
		)

		By("Should not update chart info")
		Expect(updated.Status.LastAppliedValues).NotTo(BeNil())
		Expect(updated.Status.LastAppliedChart.Version).NotTo(Equal(invalidChartVersion))
		Expect(updated.Status.LastAppliedChart.Version).To(Equal(addon.Status.LastAppliedChart.Version))

		By("Verifying no errors in controllers after update chart to invalid version")
		controller.AssertNoErrorsFor("operator-helm-controller")
	})
})
