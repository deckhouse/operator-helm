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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
	"github.com/deckhouse/operator-helm/tests/e2e/internal/controller"
	"github.com/deckhouse/operator-helm/tests/e2e/internal/framework"
	"github.com/deckhouse/operator-helm/tests/e2e/internal/util"
)

var _ = Describe("HelmClusterAddon system namespace restriction", Ordered, func() {
	f := framework.NewFramework("addon-system-ns")

	const (
		repoName  = "e2e-system-ns-repo"
		repoURL   = "https://stefanprodan.github.io/podinfo"
		addonName = "e2e-system-ns-addon"
		chartName = "podinfo"
		version   = "6.10.2"
	)

	newAddon := func(name, namespace string) *apiv1alpha1.HelmClusterAddon {
		return &apiv1alpha1.HelmClusterAddon{
			ObjectMeta: metav1.ObjectMeta{
				Name: name,
			},
			Spec: apiv1alpha1.HelmClusterAddonSpec{
				Chart: apiv1alpha1.HelmClusterAddonChartRef{
					HelmClusterAddonChartName:  chartName,
					HelmClusterAddonRepository: repoName,
					Version:                    version,
				},
				Namespace: namespace,
			},
		}
	}

	BeforeAll(func() {
		DeferCleanup(f.After)
		f.Before()
	})

	AfterEach(func() {
		By("Verifying no errors in operator-helm-controller logs")
		controller.AssertNoErrorsFor("operator-helm-controller")
	})

	DescribeTable(
		"should reject creating an addon targeting a system namespace",
		func(namespace string) {
			addon := newAddon("e2e-system-ns-create", namespace)

			_, err := f.OperatorClient().HelmV1alpha1().
				HelmClusterAddons().
				Create(context.Background(), addon, metav1.CreateOptions{})

			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("system namespace"))
		},
		Entry("kube-system", "kube-system"),
		Entry("kube-public", "kube-public"),
		Entry("kube-node-lease", "kube-node-lease"),
		Entry("d8-prefixed namespace", "d8-system"),
	)

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

		f.DeferDelete(created)

		By("Waiting for repository to become Ready")
		util.UntilConditionTrue(apiv1alpha1.ConditionTypeReady, framework.LongTimeout, created)
	})

	It("should create an addon targeting a non-system namespace", func() {
		addon := newAddon(addonName, f.NamespaceName())

		created, err := f.OperatorClient().HelmV1alpha1().
			HelmClusterAddons().
			Create(context.Background(), addon, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())

		f.DeferDelete(created)

		By("Waiting for addon to be installed")
		util.UntilConditionTrue(apiv1alpha1.ConditionTypeReady, framework.LongTimeout, created)
	})

	DescribeTable(
		"should reject updating an existing addon to a system namespace",
		func(namespace string) {
			current, err := f.OperatorClient().HelmV1alpha1().
				HelmClusterAddons().
				Get(context.Background(), addonName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())

			current.Spec.Namespace = namespace

			_, err = f.OperatorClient().HelmV1alpha1().
				HelmClusterAddons().
				Update(context.Background(), current, metav1.UpdateOptions{})

			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("system namespace"))

			By("Verifying the stored namespace was not changed")
			stored, err := f.OperatorClient().HelmV1alpha1().
				HelmClusterAddons().
				Get(context.Background(), addonName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			Expect(stored.Spec.Namespace).To(Equal(f.NamespaceName()))
		},
		Entry("kube-system", "kube-system"),
		Entry("kube-public", "kube-public"),
		Entry("kube-node-lease", "kube-node-lease"),
		Entry("d8-prefixed namespace", "d8-operator-helm"),
	)
})
