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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
	"github.com/deckhouse/operator-helm/tests/e2e/internal/controller"
	"github.com/deckhouse/operator-helm/tests/e2e/internal/framework"
	"github.com/deckhouse/operator-helm/tests/e2e/internal/util"
)

var _ = Describe("HelmApplication identity and isolation", Ordered, func() {
	f := framework.NewFramework("application-isolation")

	const (
		repoName = "e2e-isolation-repo"
		repoURL  = "https://stefanprodan.github.io/podinfo"
		appName  = "e2e-isolation-app"
	)

	BeforeAll(func() {
		DeferCleanup(f.After)
		f.Before()
	})

	AfterEach(func() {
		By("Verifying no errors in operator-helm-controller logs")
		controller.AssertNoErrorsFor("operator-helm-controller")
	})

	It("should install an application", func() {
		repo := &apiv1alpha1.HelmApplicationRepository{
			ObjectMeta: metav1.ObjectMeta{Name: repoName, Namespace: f.NamespaceName()},
			Spec:       apiv1alpha1.RepositorySpec{URL: repoURL},
		}
		createdRepo, err := f.OperatorClient().HelmV1alpha1().
			HelmApplicationRepositories(f.NamespaceName()).
			Create(context.Background(), repo, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		f.DeferDelete(createdRepo)

		util.UntilConditionTrue(apiv1alpha1.ConditionTypeReady, framework.LongTimeout, createdRepo)
		util.UntilConditionTrue(apiv1alpha1.ConditionTypeSynced, framework.LongTimeout, createdRepo)

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
		createdApp, err := f.OperatorClient().HelmV1alpha1().
			HelmApplications(f.NamespaceName()).
			Create(context.Background(), app, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		f.DeferDelete(createdApp)

		util.UntilConditionTrue(apiv1alpha1.ConditionTypeReady, framework.LongTimeout, createdApp)
	})

	It("should apply the chart as its own service account", func() {
		saName := util.ApplicationServiceAccountName(f.NamespaceName(), appName)

		By("The service account lives in the module namespace and mounts no token")
		sa, err := f.KubeClient().CoreV1().ServiceAccounts(moduleNS).
			Get(context.Background(), saName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(sa.AutomountServiceAccountToken).NotTo(BeNil())
		Expect(*sa.AutomountServiceAccountToken).To(BeFalse(),
			"the account is only a subject name; no token must be mounted for it")

		By("The role binding names that account and the namespace role")
		binding, err := f.KubeClient().RbacV1().RoleBindings(f.NamespaceName()).
			Get(context.Background(), saName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(binding.RoleRef.Kind).To(Equal("Role"))
		Expect(binding.RoleRef.Name).To(Equal(appRoleName))
		Expect(binding.Subjects).To(HaveLen(1))
		Expect(binding.Subjects[0].Name).To(Equal(saName))
		Expect(binding.Subjects[0].Namespace).To(Equal(moduleNS))
	})

	It("should create nothing else of its own in the namespace", func() {
		By("Secrets in the namespace belong to helm storage and the chart, not to the operator")
		secrets, err := f.KubeClient().CoreV1().Secrets(f.NamespaceName()).
			List(context.Background(), metav1.ListOptions{
				LabelSelector: apiv1alpha1.LabelManagedBy + "=" + apiv1alpha1.LabelManagedByValue,
			})
		Expect(err).NotTo(HaveOccurred())
		Expect(secrets.Items).To(BeEmpty(),
			"repository credentials must never be projected into a consumer namespace")

		By("The operator's own objects here are exactly the role binding and the role")
		bindings, err := f.KubeClient().RbacV1().RoleBindings(f.NamespaceName()).
			List(context.Background(), metav1.ListOptions{
				LabelSelector: apiv1alpha1.LabelManagedBy + "=" + apiv1alpha1.LabelManagedByValue,
			})
		Expect(err).NotTo(HaveOccurred())
		Expect(bindings.Items).To(HaveLen(1))

		roles, err := f.KubeClient().RbacV1().Roles(f.NamespaceName()).
			List(context.Background(), metav1.ListOptions{
				LabelSelector: apiv1alpha1.LabelManagedBy + "=" + apiv1alpha1.LabelManagedByValue,
			})
		Expect(err).NotTo(HaveOccurred())
		Expect(roles.Items).To(HaveLen(1))
		Expect(roles.Items[0].Name).To(Equal(appRoleName))
	})

	It("should leave an edited role alone and recreate a deleted one", func() {
		By("Narrowing the role the way a namespace owner would")
		narrowed := []rbacv1.PolicyRule{{
			APIGroups: []string{""},
			Resources: []string{"configmaps"},
			Verbs:     []string{"get", "list"},
		}}

		Eventually(func(g Gomega) {
			role, err := f.KubeClient().RbacV1().Roles(f.NamespaceName()).
				Get(context.Background(), appRoleName, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())

			role.Rules = narrowed
			_, err = f.KubeClient().RbacV1().Roles(f.NamespaceName()).
				Update(context.Background(), role, metav1.UpdateOptions{})
			g.Expect(err).NotTo(HaveOccurred())
		}).WithTimeout(framework.LongTimeout).WithPolling(framework.PollingInterval).Should(Succeed())

		By("Forcing a reconciliation of the application")
		util.UpdateHelmApplication(f.NamespaceName(), appName, func(app *apiv1alpha1.HelmApplication) {
			if app.Annotations == nil {
				app.Annotations = map[string]string{}
			}
			app.Annotations[apiv1alpha1.AnnotationForceReconcile] = "true"
		})

		By("The controller must not rewrite the narrowed role")
		Consistently(func(g Gomega) {
			role, err := f.KubeClient().RbacV1().Roles(f.NamespaceName()).
				Get(context.Background(), appRoleName, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(role.Rules).To(Equal(narrowed))
		}).WithTimeout(framework.ShortTimeout).WithPolling(framework.PollingInterval).Should(Succeed())

		By("Deleting the role must bring it back with full rights")
		err := f.KubeClient().RbacV1().Roles(f.NamespaceName()).
			Delete(context.Background(), appRoleName, metav1.DeleteOptions{})
		Expect(err).NotTo(HaveOccurred())

		util.UpdateHelmApplication(f.NamespaceName(), appName, func(app *apiv1alpha1.HelmApplication) {
			app.Annotations[apiv1alpha1.AnnotationForceReconcile] = "again"
		})

		Eventually(func(g Gomega) {
			role, err := f.KubeClient().RbacV1().Roles(f.NamespaceName()).
				Get(context.Background(), appRoleName, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(role.Rules).To(HaveLen(1))
			g.Expect(role.Rules[0].Verbs).To(ContainElement("*"))
		}).WithTimeout(framework.LongTimeout).WithPolling(framework.PollingInterval).Should(Succeed())
	})
})
