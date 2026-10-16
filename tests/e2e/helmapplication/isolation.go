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
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/deckhouse/operator-helm/api/naming"
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
		saName := naming.DerivedName(naming.ApplicationPrefix, apiv1alpha1.HelmApplicationKind, f.NamespaceName(), appName)

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
		Expect(binding.RoleRef.Kind).To(Equal("ClusterRole"))
		Expect(binding.RoleRef.Name).To(Equal(appClusterRoleName))
		Expect(binding.Subjects).To(HaveLen(1))
		Expect(binding.Subjects[0].Name).To(Equal(saName))
		Expect(binding.Subjects[0].Namespace).To(Equal(moduleNS))

		By("The internal HelmRelease names that account and the application namespace as its storage")
		Eventually(func(g Gomega) {
			gotServiceAccountName, gotStorageNamespace, err := util.HelmApplicationInternalReleaseSpec(f.NamespaceName(), appName)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(gotServiceAccountName).To(Equal(saName))
			g.Expect(gotStorageNamespace).To(Equal(f.NamespaceName()))
		}).WithTimeout(framework.LongTimeout).WithPolling(framework.PollingInterval).Should(Succeed())

		By("A Helm storage secret exists in the application namespace")
		Eventually(func(g Gomega) {
			secrets, err := f.KubeClient().CoreV1().Secrets(f.NamespaceName()).
				List(context.Background(), metav1.ListOptions{FieldSelector: "type=helm.sh/release.v1"})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(secrets.Items).NotTo(BeEmpty(), "a Helm release storage secret must exist in the application namespace")
		}).WithTimeout(framework.LongTimeout).WithPolling(framework.PollingInterval).Should(Succeed())
	})

	It("should create nothing else of its own in the namespace", func() {
		saName := naming.DerivedName(naming.ApplicationPrefix, apiv1alpha1.HelmApplicationKind, f.NamespaceName(), appName)

		By("Every secret in the namespace is Helm's own release storage, not a projected credential")
		secrets, err := f.KubeClient().CoreV1().Secrets(f.NamespaceName()).
			List(context.Background(), metav1.ListOptions{})
		Expect(err).NotTo(HaveOccurred())
		for _, secret := range secrets.Items {
			Expect(secret.Type).To(Equal(corev1.SecretType("helm.sh/release.v1")),
				"repository credentials must never be projected into a consumer namespace")
		}

		By("The operator's own object here is exactly the role binding: the rights come from a ClusterRole")
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
		Expect(roles.Items).To(BeEmpty(), "no namespace role must be created: the rights come from the module's ClusterRole")

		By("No ClusterRoleBinding names the application's service account as a direct subject")
		clusterBindings, err := f.KubeClient().RbacV1().ClusterRoleBindings().
			List(context.Background(), metav1.ListOptions{})
		Expect(err).NotTo(HaveOccurred())
		for _, crb := range clusterBindings.Items {
			for _, subject := range crb.Subjects {
				Expect(subject).NotTo(Equal(rbacv1.Subject{
					Kind:      rbacv1.ServiceAccountKind,
					Name:      saName,
					Namespace: moduleNS,
				}), "the application's service account must not be a direct subject of any ClusterRoleBinding")
			}
		}
	})

	It("should carry the module label on the binding", func() {
		saName := naming.DerivedName(naming.ApplicationPrefix, apiv1alpha1.HelmApplicationKind, f.NamespaceName(), appName)

		binding, err := f.KubeClient().RbacV1().RoleBindings(f.NamespaceName()).
			Get(context.Background(), saName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(binding.Labels).To(HaveKeyWithValue(apiv1alpha1.LabelManagedBy, apiv1alpha1.LabelManagedByValue))
	})

	It("should recreate a deleted binding on its own", func() {
		saName := naming.DerivedName(naming.ApplicationPrefix, apiv1alpha1.HelmApplicationKind, f.NamespaceName(), appName)

		By("Deleting the binding")
		Expect(f.KubeClient().RbacV1().RoleBindings(f.NamespaceName()).
			Delete(context.Background(), saName, metav1.DeleteOptions{})).To(Succeed())

		By("It must come back with the rights the release is applied with")
		Eventually(func(g Gomega) {
			binding, err := f.KubeClient().RbacV1().RoleBindings(f.NamespaceName()).
				Get(context.Background(), saName, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(binding.RoleRef.Kind).To(Equal("ClusterRole"))
			g.Expect(binding.RoleRef.Name).To(Equal(appClusterRoleName))
			g.Expect(binding.Subjects).To(HaveLen(1))
			g.Expect(binding.Subjects[0].Name).To(Equal(saName))
			g.Expect(binding.Subjects[0].Namespace).To(Equal(moduleNS))
		}).WithTimeout(framework.LongTimeout).WithPolling(framework.PollingInterval).Should(Succeed())
	})

	It("should keep the application identity inside the limits of its namespace", func() {
		saName := naming.DerivedName(naming.ApplicationPrefix, apiv1alpha1.HelmApplicationKind, f.NamespaceName(), appName)
		ns := f.NamespaceName()
		ctx := context.Background()

		By("Acting as the identity the release is applied with")
		cfg := framework.GetClients().RestConfig()
		cfg.Impersonate = rest.ImpersonationConfig{UserName: "system:serviceaccount:" + moduleNS + ":" + saName}
		asApp, err := kubernetes.NewForConfig(cfg)
		Expect(err).NotTo(HaveOccurred())

		By("It holds the namespace: a Role of its own and a binding to it go through")
		reader := &rbacv1.Role{
			ObjectMeta: metav1.ObjectMeta{Name: "e2e-app-reader", Namespace: ns},
			Rules:      []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"configmaps"}, Verbs: []string{"get", "list"}}},
		}
		// The binding was just recreated: the authorizer's cache may lag behind the API server.
		Eventually(func(g Gomega) {
			_, err := asApp.RbacV1().Roles(ns).Create(ctx, reader, metav1.CreateOptions{})
			g.Expect(err).NotTo(HaveOccurred())
		}).WithTimeout(framework.ShortTimeout).WithPolling(framework.PollingInterval).Should(Succeed())
		toReader := &rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "e2e-app-reader", Namespace: ns},
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: reader.Name},
			Subjects:   []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: "default", Namespace: ns}},
		}
		_, err = asApp.RbacV1().RoleBindings(ns).Create(ctx, toReader, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())

		By("The namespace object itself is out of its reach")
		_, err = asApp.CoreV1().Namespaces().Patch(ctx, ns, types.MergePatchType,
			[]byte(`{"metadata":{"labels":{"e2e":"touched"}}}`), metav1.PatchOptions{})
		Expect(apierrors.IsForbidden(err)).To(BeTrue(), "patching the namespace: %v", err)
		Expect(err.Error()).To(ContainSubstring("may not modify its namespace"))

		By("A Role granting a protected kind, in any spelling, is refused")
		for _, rule := range []rbacv1.PolicyRule{
			{APIGroups: []string{""}, Resources: []string{"resourcequotas"}, Verbs: []string{"update"}},
			{APIGroups: []string{""}, Resources: []string{"*"}, Verbs: []string{"update"}},
			{APIGroups: []string{""}, Resources: []string{"*/status"}, Verbs: []string{"update"}},
			{APIGroups: []string{""}, Resources: []string{"namespaces/finalize"}, Verbs: []string{"update"}},
			{APIGroups: []string{"deckhouse.io"}, Resources: []string{"projectnamespaces"}, Verbs: []string{"delete"}},
		} {
			escalation := &rbacv1.Role{
				ObjectMeta: metav1.ObjectMeta{Name: "e2e-app-escalation", Namespace: ns},
				Rules:      []rbacv1.PolicyRule{rule},
			}
			_, err = asApp.RbacV1().Roles(ns).Create(ctx, escalation, metav1.CreateOptions{})
			Expect(apierrors.IsForbidden(err)).To(BeTrue(), "creating a role on %v: %v", rule.Resources, err)
			Expect(err.Error()).To(ContainSubstring("may not grant write access"), "resources %v", rule.Resources)
		}

		By("A Role with a verb the identity does not hold is refused by the API server")
		wild := &rbacv1.Role{
			ObjectMeta: metav1.ObjectMeta{Name: "e2e-app-escalation", Namespace: ns},
			Rules:      []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"configmaps"}, Verbs: []string{"*"}}},
		}
		_, err = asApp.RbacV1().Roles(ns).Create(ctx, wild, metav1.CreateOptions{})
		Expect(apierrors.IsForbidden(err)).To(BeTrue(), "creating a role with every verb: %v", err)

		By("A binding to a ClusterRole is refused, its own one included")
		toCluster := &rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "e2e-app-escalation", Namespace: ns},
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: appClusterRoleName},
			Subjects:   []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: "default", Namespace: ns}},
		}
		_, err = asApp.RbacV1().RoleBindings(ns).Create(ctx, toCluster, metav1.CreateOptions{})
		Expect(apierrors.IsForbidden(err)).To(BeTrue(), "binding the application ClusterRole: %v", err)
		Expect(err.Error()).To(ContainSubstring("may reference only a Role"))

		By("Nor may it add a subject to the binding the controller owns")
		_, err = asApp.RbacV1().RoleBindings(ns).Patch(ctx, saName, types.JSONPatchType,
			[]byte(`[{"op":"add","path":"/subjects/-","value":{"kind":"ServiceAccount","name":"default","namespace":"`+ns+`"}}]`),
			metav1.PatchOptions{})
		Expect(apierrors.IsForbidden(err)).To(BeTrue(), "widening the controller's binding: %v", err)
	})
})
