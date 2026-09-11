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

package helmapplication

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

const (
	chartName   = "podinfo"
	chartVer    = "6.10.2"
	moduleNS    = "d8-operator-helm"
	appRoleName = "operator-helm-application"
)

func DefineLifecycleTests(repoType, repoURL string) {
	Describe(fmt.Sprintf("Using namespaced %s repository", repoType), Ordered, func() {
		f := framework.NewFramework("application-lifecycle")

		suffix := strings.ToLower(repoType)
		repoName := "e2e-app-repo-" + suffix
		appName := "e2e-test-app-" + suffix

		labelSelector := fmt.Sprintf("app.kubernetes.io/name=%s", chartName)

		BeforeAll(func() {
			DeferCleanup(f.After)
			f.Before()
		})

		AfterEach(func() {
			By("Verifying no errors in operator-helm-controller logs")
			controller.AssertNoErrorsFor("operator-helm-controller")
		})

		It("should create HelmApplicationRepository and reach Ready", func() {
			repo := &apiv1alpha1.HelmApplicationRepository{
				ObjectMeta: metav1.ObjectMeta{Name: repoName, Namespace: f.NamespaceName()},
				Spec:       apiv1alpha1.RepositorySpec{URL: repoURL},
			}

			created, err := f.OperatorClient().HelmV1alpha1().
				HelmApplicationRepositories(f.NamespaceName()).
				Create(context.Background(), repo, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())

			f.DeferDelete(created)

			util.UntilConditionTrue(apiv1alpha1.ConditionTypeReady, framework.LongTimeout, created)
			util.UntilConditionTrue(apiv1alpha1.ConditionTypeSynced, framework.LongTimeout, created)
		})

		It("should install the chart into the application's own namespace", func() {
			app := &apiv1alpha1.HelmApplication{
				ObjectMeta: metav1.ObjectMeta{Name: appName, Namespace: f.NamespaceName()},
				Spec: apiv1alpha1.HelmApplicationSpec{
					Chart: apiv1alpha1.HelmApplicationChartRef{
						Name:       chartName,
						Repository: repoName,
						Version:    chartVer,
					},
				},
			}

			created, err := f.OperatorClient().HelmV1alpha1().
				HelmApplications(f.NamespaceName()).
				Create(context.Background(), app, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())

			f.DeferDelete(created)

			By("Waiting for the application to become Ready")
			util.UntilConditionTrue(apiv1alpha1.ConditionTypeReady, framework.LongTimeout, created)

			By("The chart's workload must run in the application's namespace")
			util.UntilPodCount(f.NamespaceName(), labelSelector, 1, framework.LongTimeout)

			By("The application must record what it applied")
			Eventually(func(g Gomega) {
				current, err := f.OperatorClient().HelmV1alpha1().
					HelmApplications(f.NamespaceName()).
					Get(context.Background(), appName, metav1.GetOptions{})
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(current.Status.LastAppliedChart).NotTo(BeNil())
				g.Expect(current.Status.LastAppliedChart.Name).To(Equal(chartName))
				g.Expect(current.Status.LastAppliedChart.Repository).To(Equal(repoName))
				g.Expect(current.Status.LastAppliedChart.ClusterRepository).To(BeEmpty(),
					"a namespaced reference must not leave a cluster one behind")
				g.Expect(current.Status.LastAppliedChart.Version).To(Equal(chartVer))
			}).WithTimeout(framework.LongTimeout).WithPolling(framework.PollingInterval).Should(Succeed())
		})

		It("should apply values from the application spec", func() {
			const values = `{"replicaCount":2}`

			util.UpdateHelmApplication(f.NamespaceName(), appName, func(app *apiv1alpha1.HelmApplication) {
				app.Spec.Values = &apiextensionsv1.JSON{Raw: []byte(values)}
			})

			By("Waiting for the new replica count to take effect")
			util.UntilPodCount(f.NamespaceName(), labelSelector, 2, framework.LongTimeout)

			By("The application must record the applied values")
			Eventually(func(g Gomega) {
				current, err := f.OperatorClient().HelmV1alpha1().
					HelmApplications(f.NamespaceName()).
					Get(context.Background(), appName, metav1.GetOptions{})
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(current.Status.LastAppliedValues).NotTo(BeNil())
				g.Expect(current.Status.LastAppliedValues.Raw).To(MatchJSON(values))
			}).WithTimeout(framework.LongTimeout).WithPolling(framework.PollingInterval).Should(Succeed())
		})

		It("should remove the release and its identity on delete", func() {
			saName := util.ApplicationServiceAccountName(f.NamespaceName(), appName)

			util.DeleteHelmApplication(f, f.NamespaceName(), appName, framework.LongTimeout)

			By("The workload must be gone")
			util.UntilPodCount(f.NamespaceName(), labelSelector, 0, framework.LongTimeout)

			By("The service account and the role binding must be gone")
			Eventually(func(g Gomega) {
				_, err := f.KubeClient().CoreV1().ServiceAccounts(moduleNS).
					Get(context.Background(), saName, metav1.GetOptions{})
				g.Expect(err).To(HaveOccurred(), "the application's service account must be deleted")

				_, err = f.KubeClient().RbacV1().RoleBindings(f.NamespaceName()).
					Get(context.Background(), saName, metav1.GetOptions{})
				g.Expect(err).To(HaveOccurred(), "the application's role binding must be deleted")
			}).WithTimeout(framework.LongTimeout).WithPolling(framework.PollingInterval).Should(Succeed())

			By("The namespace role must survive the application")
			_, err := f.KubeClient().RbacV1().Roles(f.NamespaceName()).
				Get(context.Background(), appRoleName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred(),
				"the role belongs to the namespace and may carry the owner's edits")
		})
	})
}

var _ = Describe("HelmApplication lifecycle", Ordered, func() {
	DefineLifecycleTests("Helm", "https://stefanprodan.github.io/podinfo")
	DefineLifecycleTests("OCI", "oci://ghcr.io/stefanprodan/charts/podinfo")
})
