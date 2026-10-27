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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/deckhouse/operator-helm/api/naming"
	apiv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
	"github.com/deckhouse/operator-helm/tests/e2e/internal/controller"
	"github.com/deckhouse/operator-helm/tests/e2e/internal/framework"
	"github.com/deckhouse/operator-helm/tests/e2e/internal/util"
)

// hybridIndex is served over HTTP while the artifact it names lives in a registry.
// That is the whole point of the scenario: neither source alone exercises it.
const hybridIndex = `apiVersion: v1
entries:
  podinfo:
    - apiVersion: v2
      name: podinfo
      version: 6.7.1
      urls:
        - oci://ghcr.io/stefanprodan/charts/podinfo:6.7.1
generated: "2026-01-01T00:00:00Z"
`

// hybridIndexImage only has to serve a static file over HTTP; a rootless nginx does
// that as well as any other web server. If the e2e environment mirrors images rather
// than pulling from Docker Hub, substitute an equivalent rootless image, listening on
// 8080, from the registry the module's own dev images come from (see
// DEV_REGISTRY_DOCKER_CONFIG in tests/e2e/internal/framework/config.go).
const hybridIndexImage = "nginxinc/nginx-unprivileged:1.27-alpine"

var _ = Describe("Using a helm repository whose index publishes a version in a registry", Ordered, func() {
	f := framework.NewFramework("addon-hybrid")

	const (
		repoName  = "e2e-test-repo-hybrid"
		addonName = "e2e-test-addon-hybrid"
		chartName = "podinfo"
		indexName = "hybrid-index"
	)

	labelSelector := fmt.Sprintf("app.kubernetes.io/name=%s-%s", addonName, chartName)

	BeforeAll(func() {
		DeferCleanup(f.After)
		f.Before()
	})

	AfterEach(func() {
		By("Verifying no errors in operator-helm-controller logs")
		controller.AssertNoErrorsFor("operator-helm-controller")
	})

	It("should serve the index over HTTP from the test namespace", func() {
		configMap := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: indexName, Namespace: f.NamespaceName()},
			Data:       map[string]string{"index.yaml": hybridIndex},
		}

		// Local vars, not ptr.To: ptr is only an indirect dependency of this
		// module and importing it for a few fixture fields would needlessly
		// promote it to a direct one.
		replicas := int32(1)
		runAsNonRoot := true
		allowPrivilegeEscalation := false
		deployment := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: indexName, Namespace: f.NamespaceName()},
			Spec: appsv1.DeploymentSpec{
				Replicas: &replicas,
				Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": indexName}},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": indexName}},
					Spec: corev1.PodSpec{
						// The namespace the framework creates carries only its own e2e
						// label (see Before() in internal/framework/framework.go), so
						// whichever pod security standard the cluster enforces by
						// default applies here. Meeting "restricted" keeps the fixture
						// correct under either that or the more permissive "baseline".
						SecurityContext: &corev1.PodSecurityContext{
							RunAsNonRoot:   &runAsNonRoot,
							SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
						},
						Containers: []corev1.Container{{
							Name:  "nginx",
							Image: hybridIndexImage,
							Ports: []corev1.ContainerPort{{ContainerPort: 8080}},
							VolumeMounts: []corev1.VolumeMount{{
								Name:      "index",
								MountPath: "/usr/share/nginx/html",
							}},
							SecurityContext: &corev1.SecurityContext{
								AllowPrivilegeEscalation: &allowPrivilegeEscalation,
								Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
							},
						}},
						Volumes: []corev1.Volume{{
							Name: "index",
							VolumeSource: corev1.VolumeSource{
								ConfigMap: &corev1.ConfigMapVolumeSource{
									LocalObjectReference: corev1.LocalObjectReference{Name: indexName},
								},
							},
						}},
					},
				},
			},
		}

		service := &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: indexName, Namespace: f.NamespaceName()},
			Spec: corev1.ServiceSpec{
				Selector: map[string]string{"app": indexName},
				Ports: []corev1.ServicePort{{
					Port:       80,
					TargetPort: intstr.FromInt32(8080),
				}},
			},
		}

		Expect(f.Create(context.Background(), configMap, deployment, service)).To(Succeed())

		By("Waiting for the index to be served")
		util.UntilAllPodsReady(f.NamespaceName(), "app="+indexName, 1, framework.LongTimeout)
	})

	It("should create HelmClusterAddonRepository and reach Ready and Synced", func() {
		repo := &apiv1alpha1.HelmClusterAddonRepository{
			ObjectMeta: metav1.ObjectMeta{Name: repoName},
			Spec: apiv1alpha1.RepositorySpec{
				URL: fmt.Sprintf("http://%s.%s.svc", indexName, f.NamespaceName()),
			},
		}

		created, err := f.OperatorClient().HelmV1alpha1().
			HelmClusterAddonRepositories().
			Create(context.Background(), repo, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())

		f.DeferDelete(created)

		util.UntilConditionTrue(apiv1alpha1.ConditionTypeReady, framework.LongTimeout, created)
		util.UntilConditionTrue(apiv1alpha1.ConditionTypeSynced, framework.LongTimeout, created)
	})

	It("should record the oci reference on the discovered chart", func() {
		chart, err := f.OperatorClient().HelmV1alpha1().
			HelmClusterAddonCharts().
			Get(context.Background(), naming.HelmClusterAddonChartName(repoName, chartName), metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(chart.Status.Versions).NotTo(BeEmpty())

		Expect(chart.Status.Versions[0].Version).To(Equal("6.7.1"))
		Expect(chart.Status.Versions[0].OCIRef).To(Equal("oci://ghcr.io/stefanprodan/charts/podinfo:6.7.1"))
		// The media type is examined when the addon is deployed and is deliberately
		// not recorded in the catalog.
		Expect(chart.Status.Versions[0].MediaType).To(BeEmpty())
	})

	It("should deploy the addon from the registry", func() {
		addon := &apiv1alpha1.HelmClusterAddon{
			ObjectMeta: metav1.ObjectMeta{Name: addonName},
			Spec: apiv1alpha1.HelmClusterAddonSpec{
				Namespace: f.NamespaceName(),
				Chart: apiv1alpha1.HelmClusterAddonChartRef{
					HelmClusterAddonRepository: repoName,
					HelmClusterAddonChartName:  chartName,
					Version:                    "6.7.1",
				},
			},
		}

		created, err := f.OperatorClient().HelmV1alpha1().
			HelmClusterAddons().
			Create(context.Background(), addon, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())

		f.DeferDelete(created)

		util.UntilConditionTrue(apiv1alpha1.ConditionTypeReady, framework.LongTimeout, created)

		By("Verifying the release actually rolled out")
		util.UntilAllPodsReady(f.NamespaceName(), labelSelector, 1, framework.LongTimeout)
	})
})
