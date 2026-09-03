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

package util

import (
	"context"
	"encoding/json"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	apiv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
	"github.com/deckhouse/operator-helm/tests/e2e/internal/framework"
)

const (
	moduleName      = "operator-helm"
	moduleNamespace = "d8-operator-helm"
)

var moduleGVR = schema.GroupVersionResource{
	Group:    "deckhouse.io",
	Version:  "v1alpha1",
	Resource: "modules",
}

var moduleConfigGVR = schema.GroupVersionResource{
	Group:    "deckhouse.io",
	Version:  "v1alpha1",
	Resource: "moduleconfigs",
}

var modulePullOverrideGVR = schema.GroupVersionResource{
	Group:    "deckhouse.io",
	Version:  "v1alpha2",
	Resource: "modulepulloverrides",
}

var moduleSourceGVR = schema.GroupVersionResource{
	Group:    "deckhouse.io",
	Version:  "v1alpha1",
	Resource: "modulesources",
}

func EnsureModuleConfig(f *framework.Framework) {
	GinkgoHelper()

	c := framework.GetConfig()

	// An unknown digest fails the run instead of skipping the check. A skipped
	// verification is indistinguishable from a passing one in the output, which is
	// exactly how a suite ends up silently exercising whatever an older build left
	// behind a mutable tag.
	Expect(c.ModuleDigest).NotTo(BeEmpty(),
		"E2E_MODULE_DIGEST is not set, so the suite cannot tell which module artifact the cluster will pull. "+
			"In CI the build job supplies it; locally resolve it with "+
			"crane digest dev-registry.deckhouse.io/sys/deckhouse-oss/modules/operator-helm:$E2E_MODULE_TAG_NAME")

	moduleSource := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "deckhouse.io/v1alpha1",
			"kind":       "ModuleSource",
			"metadata": map[string]interface{}{
				"name": moduleName,
			},
			"spec": map[string]interface{}{
				"registry": map[string]interface{}{
					"ca":        "",
					"dockerCfg": c.ModuleSourceDockerCfg,
					"scheme":    "HTTPS",
					"repo":      "dev-registry.deckhouse.io/sys/deckhouse-oss/modules",
				},
			},
		},
	}

	Eventually(func(g Gomega) {
		g.Expect(f.EnsureDynamicWithoutCleanup(context.Background(), moduleSourceGVR, "", moduleSource, true)).To(Succeed())
	}).WithTimeout(framework.LongTimeout).WithPolling(framework.PollingInterval).Should(Succeed())

	untilObjectField("status.phase", "Active", framework.LongTimeout, moduleSource)

	mpo := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "deckhouse.io/v1alpha2",
			"kind":       "ModulePullOverride",
			"metadata": map[string]interface{}{
				"name": moduleName,
			},
			"spec": map[string]interface{}{
				"imageTag":     c.ModuleTagName,
				"rollback":     false,
				"scanInterval": "5s",
			},
		},
	}

	mc := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "deckhouse.io/v1alpha1",
			"kind":       "ModuleConfig",
			"metadata": map[string]interface{}{
				"name": moduleName,
			},
			"spec": map[string]interface{}{
				"enabled": true,
				"source":  c.ModuleSource,
			},
		},
	}

	Eventually(func(g Gomega) {
		g.Expect(f.EnsureDynamicWithoutCleanup(context.Background(), moduleConfigGVR, "", mc, true)).To(Succeed())
	}).WithTimeout(framework.LongTimeout).WithPolling(framework.PollingInterval).Should(Succeed())

	Eventually(func(g Gomega) {
		g.Expect(f.EnsureDynamicWithoutCleanup(context.Background(), modulePullOverrideGVR, "", mpo, true)).To(Succeed())
	}).WithTimeout(framework.LongTimeout).WithPolling(framework.PollingInterval).Should(Succeed())
}

func DisableModuleConfig(timeout time.Duration) {
	GinkgoHelper()

	moduleConfigPatchData := map[string]interface{}{
		"metadata": map[string]interface{}{
			"annotations": map[string]string{
				"modules.deckhouse.io/allow-disabling": "true",
			},
		},
		"spec": map[string]interface{}{
			"enabled": false,
		},
	}

	moduleConfigPayloadBytes, err := json.Marshal(moduleConfigPatchData)
	Expect(err).NotTo(HaveOccurred(), "failed to marshal module config annotation")

	Eventually(func(g Gomega) {
		_, err = framework.GetClients().DynamicClient().Resource(moduleConfigGVR).Patch(context.TODO(), moduleName, types.MergePatchType, moduleConfigPayloadBytes, metav1.PatchOptions{})
		g.Expect(err).NotTo(HaveOccurred(), "failed to update module config before deletion")
	}).WithTimeout(framework.LongTimeout).WithPolling(framework.PollingInterval).Should(Succeed())
}

func UntilModuleEnabled(deployAt metav1.Time, timeout time.Duration) {
	GinkgoHelper()

	var module *unstructured.Unstructured

	Eventually(func(g Gomega) {
		var err error
		module, err = framework.GetClients().DynamicClient().Resource(moduleGVR).Get(context.TODO(), moduleName, metav1.GetOptions{})
		g.Expect(err).NotTo(HaveOccurred())
	}).WithTimeout(framework.LongTimeout).WithPolling(framework.PollingInterval).Should(Succeed())

	UntilConditionStatus("IsOverridden", string(metav1.ConditionTrue), framework.MaxTimeout, module)
	UntilConditionStatusWithLastTransitionTime("EnabledByModuleConfig", string(metav1.ConditionTrue), deployAt, framework.LongTimeout, module)
	UntilConditionStatusWithLastTransitionTime("EnabledByModuleManager", string(metav1.ConditionTrue), deployAt, framework.LongTimeout, module)
	UntilConditionStatusWithLastTransitionTime("IsReady", string(metav1.ConditionTrue), deployAt, framework.MaxTimeout, module)

	// Only now is the digest meaningful: Deckhouse records which artifact the tag
	// resolved to when it actually pulls the module, and it pulls it once the
	// module is enabled. Checked here rather than right after the pull override is
	// created, where status.imageDigest is still empty.
	digestCfg := framework.GetConfig()

	By("Verifying the module pull override resolved to digest " + digestCfg.ModuleDigest)

	Eventually(func(g Gomega) {
		override, err := framework.GetClients().DynamicClient().
			Resource(modulePullOverrideGVR).
			Get(context.TODO(), moduleName, metav1.GetOptions{})
		g.Expect(err).NotTo(HaveOccurred())

		digest, found, err := unstructured.NestedString(override.Object, "status", "imageDigest")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(found).To(BeTrue(), "module pull override has no status.imageDigest yet")
		g.Expect(digest).To(Equal(digestCfg.ModuleDigest),
			"cluster is running module digest %q, expected %q", digest, digestCfg.ModuleDigest)
	}).WithTimeout(framework.LongTimeout).WithPolling(framework.PollingInterval).Should(Succeed())

	Eventually(func(g Gomega) {
		webhook, err := framework.GetClients().KubeClient().AdmissionregistrationV1().ValidatingWebhookConfigurations().Get(context.TODO(), "operator-helm-controller-admission-webhook", metav1.GetOptions{})
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(webhook.Webhooks).NotTo(BeEmpty())

		caBundle := webhook.Webhooks[0].ClientConfig.CABundle

		secret, err := framework.GetClients().KubeClient().CoreV1().Secrets(moduleNamespace).Get(context.TODO(), "operator-helm-controller-tls", metav1.GetOptions{})
		g.Expect(err).NotTo(HaveOccurred())

		caCert, found := secret.Data["ca.crt"]
		g.Expect(found).To(BeTrue())

		g.Expect(caBundle).To(Equal(caCert), "webhook caBundle does not match module ca certificate")
	}).WithTimeout(timeout).WithPolling(framework.PollingInterval).Should(Succeed())

	Eventually(func(g Gomega) {
		pods, err := framework.GetClients().KubeClient().CoreV1().
			Pods(moduleNamespace).
			List(context.Background(), metav1.ListOptions{})
		g.Expect(err).NotTo(HaveOccurred())
		activePods := notTerminating(pods.Items)
		g.Expect(activePods).NotTo(BeEmpty(),
			"no pods found in namespace %s", moduleNamespace)

		for _, pod := range activePods {
			g.Expect(pod.CreationTimestamp.After(deployAt.UTC().Add(-1*time.Second))).To(BeTrue(),
				"pod was created at %v, which is not after %v", pod.CreationTimestamp, deployAt)
			g.Expect(pod.Status.Phase).To(Equal(corev1.PodRunning),
				"pod %s is %s, not Running", pod.Name, pod.Status.Phase)
			for _, cs := range pod.Status.ContainerStatuses {
				g.Expect(cs.Ready).To(BeTrue(),
					"container %s in pod %s is not ready", cs.Name, pod.Name)
			}
		}
	}).WithTimeout(timeout).WithPolling(framework.PollingInterval).Should(Succeed())

	Eventually(func(g Gomega) {
		err := framework.GetClients().KubeClient().CoreV1().Pods("d8-system").DeleteCollection(context.TODO(), metav1.DeleteOptions{}, metav1.ListOptions{LabelSelector: "app=webhook-handler"})
		g.Expect(err).NotTo(HaveOccurred(), "should remove deckhouse webhook-handler pods in d8-system namespace")
	}).WithTimeout(framework.ShortTimeout).WithPolling(framework.PollingInterval).Should(Succeed())

	UntilAllPodsReady("d8-system", "app=webhook-handler", 1, timeout)

	// The delete above may still be tearing down the old pod when this Consistently
	// starts: the terminating pod lingers in List results alongside the already-Running
	// replacement UntilAllPodsReady just confirmed, so it must be filtered out here too
	// or a single healthy pod counts as two.
	Consistently(func(g Gomega) {
		pods, err := framework.GetClients().KubeClient().CoreV1().
			Pods("d8-system").
			List(context.Background(), metav1.ListOptions{LabelSelector: "app=webhook-handler"})
		g.Expect(err).NotTo(HaveOccurred())
		activePods := notTerminating(pods.Items)
		g.Expect(len(activePods)).To(Equal(1),
			"expected %d pods, got %d", 1, len(activePods))

		for _, pod := range activePods {
			g.Expect(pod.Status.Phase).To(Equal(corev1.PodRunning),
				"pod %s phase: %s", pod.Name, pod.Status.Phase)
			for _, cs := range pod.Status.ContainerStatuses {
				g.Expect(cs.Ready).To(BeTrue(),
					"pod %s container %s not ready", pod.Name, cs.Name)
			}
		}
	}, "60s", "1s").Should(Succeed())

	// The caBundle check above only proves that two API objects agree with each
	// other: the ValidatingWebhookConfiguration's caBundle equals ca.crt in the
	// controller's TLS Secret. It does not prove the API server can actually
	// complete a TLS handshake with the certificate the running webhook pod
	// serves. Only HelmClusterAddon goes through that webhook, so a dry-run
	// create of one is the faithful, side-effect-free way to exercise the real
	// admission path before any spec relies on it.
	By("Verifying the HelmClusterAddon validating webhook is reachable")

	probe := &apiv1alpha1.HelmClusterAddon{
		ObjectMeta: metav1.ObjectMeta{
			Name: "e2e-webhook-probe-preflight",
		},
		Spec: apiv1alpha1.HelmClusterAddonSpec{
			Chart: apiv1alpha1.HelmClusterAddonChartRef{
				HelmClusterAddonChartName:  "e2e-webhook-probe",
				HelmClusterAddonRepository: "e2e-webhook-probe",
				Version:                    "0.0.0",
			},
			Namespace: "default",
		},
	}

	Eventually(func(g Gomega) {
		_, err := framework.GetClients().OperatorClient().HelmV1alpha1().
			HelmClusterAddons().
			Create(context.TODO(), probe, metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}})

		// Schema validation runs before admission webhooks in the API server's
		// pipeline, so an Invalid response means the request never reached the
		// webhook at all. That is not "not ready yet" — it means the probe object
		// above has drifted from the CRD's own constraints (a new required field,
		// a tightened MinLength, ...) and this check has stopped proving anything
		// about the webhook. Fail the setup immediately rather than retrying a
		// broken probe until the timeout.
		if apierrors.IsInvalid(err) {
			Expect(err).NotTo(HaveOccurred(),
				"the webhook-reachability probe object is no longer valid against the "+
					"HelmClusterAddon CRD (%v); fix the probe built in UntilModuleEnabled "+
					"to satisfy the current CRD constraints — as written it can no longer "+
					"prove the validating webhook is reachable", err)
		}

		if err == nil || !apierrors.IsInternalError(err) {
			// Either the dry-run create was admitted, or it was rejected with a
			// verdict that only the webhook itself could have produced (e.g. a
			// namespace or uniqueness violation). Both prove the webhook was
			// actually called, which is all this probe needs to establish.
			return
		}

		g.Expect(err).NotTo(HaveOccurred(),
			"the HelmClusterAddon validating webhook is still not reachable: %v. "+
				"This is usually caused by the webhook serving a certificate the "+
				"API server does not yet trust, even though the caBundle/ca.crt "+
				"check above already passed.", err)
	}).WithTimeout(timeout).WithPolling(framework.PollingInterval).Should(Succeed())
}

func UntilModuleDisabled(timeout time.Duration) {
	GinkgoHelper()

	module, err := framework.GetClients().DynamicClient().Resource(moduleGVR).Get(context.TODO(), moduleName, metav1.GetOptions{})
	Expect(err).NotTo(HaveOccurred())
	UntilConditionStatus("EnabledByModuleConfig", string(metav1.ConditionFalse), framework.LongTimeout, module)
	UntilConditionStatus("EnabledByModuleManager", string(metav1.ConditionFalse), framework.LongTimeout, module)
	UntilConditionStatus("IsReady", string(metav1.ConditionFalse), framework.LongTimeout, module)

	Eventually(func(g Gomega) {
		pods, err := framework.GetClients().KubeClient().CoreV1().
			Pods(moduleNamespace).
			List(context.Background(), metav1.ListOptions{})
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(pods.Items).To(BeEmpty(),
			"pods still exist in namespace %s", moduleNamespace)
	}).WithTimeout(timeout).WithPolling(framework.PollingInterval).Should(Succeed())
}
