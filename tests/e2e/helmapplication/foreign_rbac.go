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
	"github.com/deckhouse/operator-helm/tests/e2e/internal/framework"
	"github.com/deckhouse/operator-helm/tests/e2e/internal/util"
)

// The name of the namespace Role is fixed and computable by anyone, so a namespace
// owner can be holding it before any application is created. Widening such a Role to
// full rights and binding every application of the namespace to it is not a decision
// the controller gets to make on their behalf: it reports the collision and installs
// nothing. The verdict is terminal because the informer behind the watch on the kind
// selects on the label the object lacks — nothing observes it arriving or leaving.
//
// No AssertNoErrorsFor here on purpose: the scenario blocks the application
// deliberately, so error-level log lines from the controller are expected. Dropping
// the assertion is not enough on its own — the log watcher accumulates errors
// suite-wide and never resets, so every other scenario's assertion would fail too.
// The message is excluded in default_config.yaml; keep the two in step.
var _ = Describe("HelmApplication over a foreign namespace role", Ordered, func() {
	f := framework.NewFramework("application-foreign-rbac")

	const (
		repoName = "e2e-foreign-rbac-repo"
		repoURL  = "https://stefanprodan.github.io/podinfo"
		appName  = "e2e-foreign-rbac-app"
	)

	var createdApp *apiv1alpha1.HelmApplication

	foreignRules := []rbacv1.PolicyRule{{
		APIGroups: []string{""},
		Resources: []string{"configmaps"},
		Verbs:     []string{"get"},
	}}

	BeforeAll(func() {
		DeferCleanup(f.After)
		f.Before()
	})

	It("should refuse to seize a role it does not own", func() {
		By("Putting a role of someone else's under the name the identity needs")
		role := &rbacv1.Role{
			ObjectMeta: metav1.ObjectMeta{Name: appRoleName, Namespace: f.NamespaceName()},
			Rules:      foreignRules,
		}
		createdRole, err := f.KubeClient().RbacV1().Roles(f.NamespaceName()).
			Create(context.Background(), role, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		f.DeferDelete(createdRole)

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

		By("The role must be left exactly as it was")
		stored, err := f.KubeClient().RbacV1().Roles(f.NamespaceName()).
			Get(context.Background(), appRoleName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(stored.Rules).To(Equal(foreignRules), "a role that is not ours must not be widened")
		Expect(stored.Labels).NotTo(HaveKey(apiv1alpha1.LabelManagedBy),
			"a role that is not ours must not be labelled as ours")

		By("Nothing must be installed while the identity cannot be built")
		_, err = f.KubeClient().RbacV1().RoleBindings(f.NamespaceName()).
			Get(context.Background(), util.ApplicationServiceAccountName(f.NamespaceName(), appName), metav1.GetOptions{})
		Expect(err).To(HaveOccurred(), "no binding must be created for a stalled identity")
	})

	It("should take the role over once it is labelled as the module's", func() {
		By("Handing the role to the module")
		role, err := f.KubeClient().RbacV1().Roles(f.NamespaceName()).
			Get(context.Background(), appRoleName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())

		if role.Labels == nil {
			role.Labels = map[string]string{}
		}
		role.Labels[apiv1alpha1.LabelManagedBy] = apiv1alpha1.LabelManagedByValue
		_, err = f.KubeClient().RbacV1().Roles(f.NamespaceName()).
			Update(context.Background(), role, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred())

		// Labelling the role makes it enter the label-scoped informer, which is what
		// wakes the application: no force request is needed for this direction.
		By("The application must recover on its own")
		util.UntilConditionAbsent(apiv1alpha1.ConditionTypeStalled, framework.LongTimeout, createdApp)
		util.UntilConditionTrue(apiv1alpha1.ConditionTypeReady, framework.LongTimeout, createdApp)

		By("The adopted role must be reconciled to full rights")
		Eventually(func(g Gomega) {
			stored, err := f.KubeClient().RbacV1().Roles(f.NamespaceName()).
				Get(context.Background(), appRoleName, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(stored.Rules).To(HaveLen(1))
			g.Expect(stored.Rules[0].Verbs).To(ContainElement("*"))
		}).WithTimeout(framework.LongTimeout).WithPolling(framework.PollingInterval).Should(Succeed())
	})
})
