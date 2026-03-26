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

package helmclusteraddonrepository

import (
	"context"
	"fmt"
	"os"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
	"github.com/deckhouse/operator-helm/tests/e2e/internal/controller"
	"github.com/deckhouse/operator-helm/tests/e2e/internal/framework"
	"github.com/deckhouse/operator-helm/tests/e2e/internal/util"
)

var _ = Describe("HelmClusterAddonRepository lifecycle", Ordered, func() {
	f := framework.NewFramework("repository-lifecycle")
	cfg := framework.GetConfig()

	const (
		repoName = "e2e-test-repo"
	)

	repoURL := os.Getenv("E2E_REPOSITORY_URL")

	BeforeAll(func() {
		DeferCleanup(f.After)
		f.Before()

		By("Verifying repository url is set")
		Expect(repoURL).NotTo(BeEmpty())

		By("Verifying all controllers are running")
		for _, ctrl := range cfg.Controllers {
			util.UntilControllerReady(ctrl.Namespace, ctrl.LabelSelector, framework.LongTimeout)
		}
	})

	It("should create HelmClusterAddonRepository", func() {
		repo := &apiv1alpha1.HelmClusterAddonRepository{
			ObjectMeta: metav1.ObjectMeta{
				Name: repoName,
			},
			Spec: apiv1alpha1.HelmClusterAddonRepositorySpec{
				URL:       repoURL,
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

		By("Should have existing HelmClusterAddonChart")
		labelSelector := fmt.Sprintf("repository=%s", repoName)
		charts, err := f.OperatorClient().
			HelmV1alpha1().
			HelmClusterAddonCharts().
			List(context.Background(), metav1.ListOptions{LabelSelector: labelSelector})
		Expect(err).NotTo(HaveOccurred())
		Expect(len(charts.Items)).To(BeNumerically(">=", 1),
			"waiting for >= %d charts, got %d", 1, len(charts.Items))

		By("HelmClusterAddonChart should have versions")
		for _, chart := range charts.Items {
			Expect(chart.Status.Versions).NotTo(BeEmpty())
		}

		By("Verifying no errors in controllers after repo creation")
		controller.AssertNoErrorsFor("operator-helm-controller")
	})
})
