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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
	"github.com/deckhouse/operator-helm/tests/e2e/internal/framework"
)

var _ = Describe("HelmApplication system namespace restriction", func() {
	f := framework.NewFramework("")

	newApplication := func(namespace string) *apiv1alpha1.HelmApplication {
		return &apiv1alpha1.HelmApplication{
			ObjectMeta: metav1.ObjectMeta{Name: "e2e-system-ns-app", Namespace: namespace},
			Spec: apiv1alpha1.HelmApplicationSpec{
				Chart: apiv1alpha1.HelmApplicationChartRef{
					Name:       chartName,
					Repository: "any-repo",
					Version:    chartVer,
				},
			},
		}
	}

	DescribeTable(
		"should reject an application in a system namespace",
		func(namespace string) {
			_, err := f.OperatorClient().HelmV1alpha1().
				HelmApplications(namespace).
				Create(context.Background(), newApplication(namespace), metav1.CreateOptions{})

			Expect(err).To(HaveOccurred(), "an application in %q must be rejected", namespace)
			Expect(err.Error()).To(ContainSubstring("system namespace"))
		},
		Entry("kube-system", "kube-system"),
		Entry("kube-public", "kube-public"),
		Entry("kube-node-lease", "kube-node-lease"),
		Entry("the module's own namespace", moduleNS),
	)
})
