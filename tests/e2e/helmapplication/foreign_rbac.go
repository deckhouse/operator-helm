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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/deckhouse/operator-helm/api/naming"
	apiv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
	"github.com/deckhouse/operator-helm/tests/e2e/internal/controller"
	"github.com/deckhouse/operator-helm/tests/e2e/internal/framework"
	"github.com/deckhouse/operator-helm/tests/e2e/internal/util"
)

var _ = Describe("HelmApplication over a foreign role binding", Ordered, func() {
	f := framework.NewFramework("application-foreign-rbac")

	const (
		repoName = "e2e-foreign-rbac-repo"
		repoURL  = "https://stefanprodan.github.io/podinfo"
		appName  = "e2e-foreign-rbac-app"
	)

	var (
		createdApp  *apiv1alpha1.HelmApplication
		bindingName string
	)

	foreignRef := rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: "someone-elses-role"}
	foreignSubjects := []rbacv1.Subject{{APIGroup: rbacv1.GroupName, Kind: rbacv1.UserKind, Name: "someone-else"}}

	BeforeAll(func() {
		DeferCleanup(f.After)
		f.Before()
		bindingName = naming.DerivedName(naming.ApplicationPrefix, apiv1alpha1.HelmApplicationKind, f.NamespaceName(), appName)
	})

	AfterEach(func() {
		By("Verifying no errors in operator-helm-controller logs")
		controller.AssertNoErrorsFor("operator-helm-controller")
	})

	It("should refuse to seize a role binding it does not own", func() {
		By("Putting a role binding of someone else's under the name the identity needs")
		binding := &rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: bindingName, Namespace: f.NamespaceName()},
			RoleRef:    foreignRef,
			Subjects:   foreignSubjects,
		}
		_, err := f.KubeClient().RbacV1().RoleBindings(f.NamespaceName()).
			Create(context.Background(), binding, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())

		By("Creating the repository and the application over it")
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
		createdApp, err = f.OperatorClient().HelmV1alpha1().
			HelmApplications(f.NamespaceName()).
			Create(context.Background(), app, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		f.DeferDelete(createdApp)

		By("The application must stall and name the collision")
		util.UntilConditionTrue(apiv1alpha1.ConditionTypeStalled, framework.LongTimeout, createdApp)
		util.UntilConditionReason(
			apiv1alpha1.ConditionTypeStalled,
			apiv1alpha1.ReasonForeignRBACObject,
			framework.LongTimeout,
			createdApp,
		)
		util.UntilConditionStatus(
			apiv1alpha1.ConditionTypeReady,
			string(metav1.ConditionFalse),
			framework.LongTimeout,
			createdApp,
		)

		By("The role binding must be left exactly as it was")
		stored, err := f.KubeClient().RbacV1().RoleBindings(f.NamespaceName()).
			Get(context.Background(), bindingName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(stored.RoleRef).To(Equal(foreignRef), "a binding that is not ours must not be re-pointed")
		Expect(stored.Subjects).To(Equal(foreignSubjects), "a binding that is not ours must keep its subjects")
		Expect(stored.Labels).NotTo(HaveKey(apiv1alpha1.LabelManagedBy),
			"a binding that is not ours must not be labelled as ours")

		By("Nothing must be installed while the identity cannot be built")
		Consistently(func(g Gomega) {
			_, _, err := util.HelmApplicationInternalReleaseSpec(f.NamespaceName(), appName)
			g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "no release must be created for a stalled identity")
		}).WithTimeout(framework.ShortTimeout).WithPolling(framework.PollingInterval).Should(Succeed())
	})

	It("should take the role binding over once it is labelled as the module's", func() {
		By("Handing the role binding to the module")
		Eventually(func(g Gomega) {
			binding, err := f.KubeClient().RbacV1().RoleBindings(f.NamespaceName()).
				Get(context.Background(), bindingName, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())

			if binding.Labels == nil {
				binding.Labels = map[string]string{}
			}
			binding.Labels[apiv1alpha1.LabelManagedBy] = apiv1alpha1.LabelManagedByValue
			_, err = f.KubeClient().RbacV1().RoleBindings(f.NamespaceName()).
				Update(context.Background(), binding, metav1.UpdateOptions{})
			g.Expect(err).NotTo(HaveOccurred())
		}).WithTimeout(framework.LongTimeout).WithPolling(framework.PollingInterval).Should(Succeed())

		By("The application must recover on its own")
		util.UntilConditionAbsent(apiv1alpha1.ConditionTypeStalled, framework.LongTimeout, createdApp)
		util.UntilConditionTrue(apiv1alpha1.ConditionTypeReady, framework.LongTimeout, createdApp)

		By("The adopted binding must be reconciled to the namespace admin ClusterRole and account")
		Eventually(func(g Gomega) {
			stored, err := f.KubeClient().RbacV1().RoleBindings(f.NamespaceName()).
				Get(context.Background(), bindingName, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(stored.RoleRef.Kind).To(Equal("ClusterRole"))
			g.Expect(stored.RoleRef.Name).To(BeElementOf(appClusterRoleNames))
			g.Expect(stored.Subjects).To(HaveLen(1))
			g.Expect(stored.Subjects[0].Kind).To(Equal(rbacv1.ServiceAccountKind))
			g.Expect(stored.Subjects[0].Name).To(Equal(bindingName))
			g.Expect(stored.Subjects[0].Namespace).To(Equal(moduleNS))
		}).WithTimeout(framework.LongTimeout).WithPolling(framework.PollingInterval).Should(Succeed())
	})
})
