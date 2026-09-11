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

package helmapplicationrepository

import (
	"context"
	"fmt"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
	"github.com/deckhouse/operator-helm/tests/e2e/internal/controller"
	"github.com/deckhouse/operator-helm/tests/e2e/internal/framework"
	"github.com/deckhouse/operator-helm/tests/e2e/internal/util"
)

func DefineLifecycleTests(repoType, repoURL string) {
	Describe(fmt.Sprintf("Testing namespaced %s repository", repoType), Ordered, func() {
		f := framework.NewFramework("app-repository-lifecycle")

		repoName := "e2e-app-repo-" + strings.ToLower(repoType)

		BeforeAll(func() {
			DeferCleanup(f.After)
			f.Before()
		})

		AfterEach(func() {
			By("Verifying no errors in operator-helm-controller logs")
			controller.AssertNoErrorsFor("operator-helm-controller")
		})

		It("should create HelmApplicationRepository and fill its catalog", func() {
			repo := &apiv1alpha1.HelmApplicationRepository{
				ObjectMeta: metav1.ObjectMeta{
					Name:      repoName,
					Namespace: f.NamespaceName(),
				},
				Spec: apiv1alpha1.RepositorySpec{
					URL:                repoURL,
					InsecureSkipVerify: false,
				},
			}

			created, err := f.OperatorClient().HelmV1alpha1().
				HelmApplicationRepositories(f.NamespaceName()).
				Create(context.Background(), repo, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())

			f.DeferDelete(created)

			By("Waiting for repository to become Ready")
			util.UntilConditionTrue(apiv1alpha1.ConditionTypeReady, framework.LongTimeout, created)

			By("Waiting for repository to become Synced")
			util.UntilConditionTrue(apiv1alpha1.ConditionTypeSynced, framework.LongTimeout, created)

			By("Healthy repository must carry no abnormal-true conditions")
			util.UntilConditionAbsent(apiv1alpha1.ConditionTypeReconciling, framework.LongTimeout, created)
			util.UntilConditionAbsent(apiv1alpha1.ConditionTypeStalled, framework.LongTimeout, created)

			By("Repository must report its synchronization schedule")
			Eventually(func(g Gomega) {
				current, err := f.OperatorClient().HelmV1alpha1().
					HelmApplicationRepositories(f.NamespaceName()).
					Get(context.Background(), repoName, metav1.GetOptions{})
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(current.Status.LastSuccessfulSyncTime).NotTo(BeNil())
				g.Expect(current.Status.NextSyncTime).NotTo(BeNil())
				g.Expect(current.Status.ConsecutiveFetchFailures).To(BeZero())
			}).WithTimeout(framework.LongTimeout).WithPolling(framework.PollingInterval).Should(Succeed())

			By("The catalog must appear in the repository's own namespace")
			labelSelector := fmt.Sprintf("repository=%s", repoName)
			Eventually(func(g Gomega) {
				charts, err := f.OperatorClient().HelmV1alpha1().
					HelmApplicationCharts(f.NamespaceName()).
					List(context.Background(), metav1.ListOptions{LabelSelector: labelSelector})
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(charts.Items).NotTo(BeEmpty(), "the repository must publish at least one chart")

				usable := 0
				for _, chart := range charts.Items {
					for _, version := range chart.Status.Versions {
						if version.UnavailableReason == "" {
							usable++
						}
					}
				}
				g.Expect(usable).To(BeNumerically(">=", 1), "the catalog must carry at least one usable version")
			}).WithTimeout(framework.LongTimeout).WithPolling(framework.PollingInterval).Should(Succeed())

			By("No catalog object of this repository may appear cluster-wide")
			Consistently(func(g Gomega) {
				clusterCharts, err := f.OperatorClient().HelmV1alpha1().
					HelmClusterApplicationCharts().
					List(context.Background(), metav1.ListOptions{LabelSelector: labelSelector})
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(clusterCharts.Items).To(BeEmpty(),
					"a namespaced repository must not publish into the cluster-wide catalog")
			}).WithTimeout(framework.ShortTimeout).WithPolling(framework.PollingInterval).Should(Succeed())
		})

		It("should keep a same-named repository in another namespace apart", func() {
			other := util.EnsureNamespace(f.NamespaceName()+"-other", map[string]string{framework.E2ELabel: "true"})
			DeferCleanup(func() {
				if framework.IsCleanUpNeeded() {
					util.DeleteNamespace(other.Name, true, framework.LongTimeout)
				}
			})

			twin := &apiv1alpha1.HelmApplicationRepository{
				ObjectMeta: metav1.ObjectMeta{
					Name:      repoName,
					Namespace: other.Name,
				},
				Spec: apiv1alpha1.RepositorySpec{URL: repoURL},
			}

			createdTwin, err := f.OperatorClient().HelmV1alpha1().
				HelmApplicationRepositories(other.Name).
				Create(context.Background(), twin, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())

			By("Both repositories must reach Ready independently")
			util.UntilConditionTrue(apiv1alpha1.ConditionTypeReady, framework.LongTimeout, createdTwin)

			labelSelector := fmt.Sprintf("repository=%s", repoName)
			By("The twin must publish its own catalog into its own namespace")
			Eventually(func(g Gomega) {
				charts, err := f.OperatorClient().HelmV1alpha1().
					HelmApplicationCharts(other.Name).
					List(context.Background(), metav1.ListOptions{LabelSelector: labelSelector})
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(charts.Items).NotTo(BeEmpty(), "the twin must publish its own catalog")
			}).WithTimeout(framework.LongTimeout).WithPolling(framework.PollingInterval).Should(Succeed())

			By("Deleting the twin must leave the original healthy")
			util.DeleteHelmApplicationRepository(f, other.Name, repoName, framework.LongTimeout)

			Consistently(func(g Gomega) {
				current, err := f.OperatorClient().HelmV1alpha1().
					HelmApplicationRepositories(f.NamespaceName()).
					Get(context.Background(), repoName, metav1.GetOptions{})
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(current.Status.Conditions).To(ContainElement(And(
					HaveField("Type", apiv1alpha1.ConditionTypeReady),
					HaveField("Status", metav1.ConditionTrue),
				)))

				charts, err := f.OperatorClient().HelmV1alpha1().
					HelmApplicationCharts(f.NamespaceName()).
					List(context.Background(), metav1.ListOptions{LabelSelector: labelSelector})
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(charts.Items).NotTo(BeEmpty(), "the original's catalog must survive the twin's deletion")
			}).WithTimeout(framework.ShortTimeout).WithPolling(framework.PollingInterval).Should(Succeed())

			// Only a helm repository owns an internal HelmRepository; an oci:// one
			// has none of its own, its artifacts come from the consumers' own
			// OCIRepository objects.
			if strings.EqualFold(repoType, "helm") {
				By("The original's internal HelmRepository must survive the twin's deletion")
				_, err = util.GetHelmApplicationRepositoryInternalHelmRepository(
					util.HelmApplicationRepositoryInternalName(f.NamespaceName(), repoName))
				Expect(err).NotTo(HaveOccurred())
			}
		})
	})
}

var _ = Describe("HelmApplicationRepository lifecycle", Ordered, func() {
	DefineLifecycleTests("Helm", "https://stefanprodan.github.io/podinfo")
	DefineLifecycleTests("OCI", "oci://ghcr.io/stefanprodan/charts/podinfo")
})
