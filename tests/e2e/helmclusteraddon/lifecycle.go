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
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
	"github.com/deckhouse/operator-helm/tests/e2e/internal/controller"
	"github.com/deckhouse/operator-helm/tests/e2e/internal/framework"
	"github.com/deckhouse/operator-helm/tests/e2e/internal/util"
)

func DefineLifecycleTests(repoType, repoURL string) {
	Describe(fmt.Sprintf("Using %s repository", repoType), Ordered, func() {
		f := framework.NewFramework("addon-lifecycle")
		cfg := framework.GetConfig()

		repoName := "e2e-test-repo-" + strings.ToLower(repoType)
		addonName := "e2e-test-addon-" + strings.ToLower(repoType)
		chartName := "podinfo"

		labelSelector := fmt.Sprintf("app.kubernetes.io/name=%s-%s", addonName, chartName)

		BeforeAll(func() {
			DeferCleanup(f.After)
			f.Before()

			deployAt := metav1.Now()

			By("Creating ModuleConfig for operator-helm")
			util.EnsureModuleConfig(f)

			By("Waiting for module pods to be Running and Ready")
			util.UntilModuleEnabled(deployAt, framework.LongTimeout)

			By("Verifying all controllers are running")
			for _, ctrl := range cfg.Controllers {
				util.UntilControllerReady(ctrl.Namespace, ctrl.LabelSelector, framework.LongTimeout)
			}
		})

		AfterAll(func() {
			if framework.IsCleanUpNeeded() {
				By("Cleaning up HelmClusterAddon")
				util.DeleteHelmClusterAddon(f, addonName, framework.LongTimeout)
				By("Cleaning up HelmClusterAddonRepositories")
				util.DeleteHelmClusterAddonRepository(f, repoName, repoType, framework.LongTimeout)
				By("Cleaning up module")
				util.DisableModuleConfig(framework.LongTimeout)
				util.UntilModuleDisabled(framework.LongTimeout)
			}
		})

		AfterEach(func() {
			By("Verifying no errors in operator-helm-controller logs")
			controller.AssertNoErrorsFor("operator-helm-controller")
		})

		It("should create HelmClusterAddonRepository and reach Ready", func() {
			repo := &apiv1alpha1.HelmClusterAddonRepository{
				ObjectMeta: metav1.ObjectMeta{
					Name: repoName,
				},
				Spec: apiv1alpha1.HelmClusterAddonRepositorySpec{
					URL:                repoURL,
					InsecureSkipVerify: false,
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
				apiv1alpha1.ConditionTypeUpdateInstalled,
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

			By("Verifying pods are still running after update")
			util.UntilPodCount(f.NamespaceName(), labelSelector, 1, framework.LongTimeout)
		})

		It("should update last applied values", func() {
			expectedValues := `{"replicaCount": 2}`

			By("Updating last applied values")
			updated := util.UpdateHelmClusterAddon(addonName, func(addon *apiv1alpha1.HelmClusterAddon) {
				addon.Spec.Values = &apiextensionsv1.JSON{Raw: []byte(expectedValues)}
			})

			By("Waiting for update to be applied")
			util.UntilConditionTrue(
				apiv1alpha1.ConditionTypeConfigurationApplied,
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

			By("Verifying pods number changed after values update")
			util.UntilPodCount(f.NamespaceName(), labelSelector, 2, framework.LongTimeout)
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

			invalidChartVersion := "invalid-version"

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
			updated, err = f.OperatorClient().HelmV1alpha1().
				HelmClusterAddons().
				Get(context.Background(), addonName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			Expect(updated.Status.LastAppliedValues).NotTo(BeNil())
			Expect(updated.Status.LastAppliedChart.Version).NotTo(Equal(invalidChartVersion))

			By("Verifying pods number changed after invalid chart info set")
			util.UntilPodCount(f.NamespaceName(), labelSelector, 2, framework.LongTimeout)
		})

		It("Should redeem on reverting chart version", func() {
			addon, err := f.OperatorClient().HelmV1alpha1().
				HelmClusterAddons().
				Get(context.Background(), addonName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())

			By("Should have PartiallyDegraded condition active")
			util.UntilConditionTrue(
				apiv1alpha1.ConditionTypePartiallyDegraded,
				framework.LongTimeout,
				addon,
			)

			validChartVersion := "6.10.2"

			By("Updating addon chart to invalid version")
			updated := util.UpdateHelmClusterAddon(addonName, func(addon *apiv1alpha1.HelmClusterAddon) {
				addon.Spec.Chart.Version = validChartVersion
			})

			By("Waiting for addon to be upgraded")
			util.UntilConditionTrue(
				apiv1alpha1.ConditionTypeReady,
				framework.LongTimeout,
				updated,
			)

			By("Should have PartiallyDegraded condition inactive")
			util.UntilConditionStatus(
				apiv1alpha1.ConditionTypePartiallyDegraded,
				string(metav1.ConditionFalse),
				framework.LongTimeout,
				updated,
			)

			By("Should update chart info")
			updated, err = f.OperatorClient().HelmV1alpha1().
				HelmClusterAddons().
				Get(context.Background(), addonName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			Expect(updated.Status.LastAppliedValues).NotTo(BeNil())
			Expect(updated.Status.LastAppliedChart.Version).To(Equal(validChartVersion))

			By("Verifying pods number changed after invalid chart info set")
			util.UntilPodCount(f.NamespaceName(), labelSelector, 2, framework.LongTimeout)
		})

		It("Should fail on invalid values set", func() {
			invalidValues := `{"replicaCount": "no"}`

			By("Updating addon chart version")
			updated := util.UpdateHelmClusterAddon(addonName, func(addon *apiv1alpha1.HelmClusterAddon) {
				addon.Spec.Values = &apiextensionsv1.JSON{Raw: []byte(invalidValues)}
			})

			By("Waiting for update to be applied")
			util.UntilConditionStatus(
				apiv1alpha1.ConditionTypeReady,
				string(metav1.ConditionFalse),
				framework.LongTimeout,
				updated,
			)

			By("Verifying invalid values were not applied")
			updated, err := f.OperatorClient().HelmV1alpha1().
				HelmClusterAddons().
				Get(context.Background(), addonName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			Expect(updated.Status.LastAppliedValues).NotTo(BeNil())
			Expect(updated.Status.LastAppliedValues.Raw).NotTo(MatchJSON(invalidValues))

			By("Verifying pods are still running after update")
			util.UntilPodCount(f.NamespaceName(), labelSelector, 2, framework.LongTimeout)
		})

		It("Should redeem on reverting values", func() {
			validValues := `{"replicaCount": 3}`

			By("Updating addon chart version")
			updated := util.UpdateHelmClusterAddon(addonName, func(addon *apiv1alpha1.HelmClusterAddon) {
				addon.Spec.Values = &apiextensionsv1.JSON{Raw: []byte(validValues)}
			})

			By("Waiting for update to be applied")
			util.UntilConditionTrue(
				apiv1alpha1.ConditionTypeConfigurationApplied,
				framework.LongTimeout,
				updated,
			)

			By("Verifying valid values were applied")
			updated, err := f.OperatorClient().HelmV1alpha1().
				HelmClusterAddons().
				Get(context.Background(), addonName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			Expect(updated.Status.LastAppliedValues).NotTo(BeNil())
			Expect(updated.Status.LastAppliedValues.Raw).To(MatchJSON(validValues))

			By("Verifying pods are running after update")
			util.UntilPodCount(f.NamespaceName(), labelSelector, 3, framework.LongTimeout)
		})
	})
}

var _ = Describe("HelmClusterAddon lifecycle", Ordered, func() {
	DefineLifecycleTests("Helm", "https://stefanprodan.github.io/podinfo")
	DefineLifecycleTests("OCI", "oci://ghcr.io/stefanprodan/charts/podinfo")
})
