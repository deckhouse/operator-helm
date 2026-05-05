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
	"time"

	"encoding/json"
	"github.com/deckhouse/operator-helm/tests/e2e/internal/framework"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
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

	Eventually(func(g Gomega) {
		g.Expect(f.EnsureDynamicWithoutCleanup(context.Background(), modulePullOverrideGVR, "", mpo, true)).To(Succeed())
	}).WithTimeout(framework.LongTimeout).WithPolling(framework.PollingInterval).Should(Succeed())

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

	Eventually(func(g Gomega) {
		webhook, err := framework.GetClients().KubeClient().AdmissionregistrationV1().ValidatingWebhookConfigurations().Get(context.TODO(), "operator-helm-controller-admission-webhook", metav1.GetOptions{})
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(webhook.CreationTimestamp.After(deployAt.UTC().Add(-1 * time.Second))).To(BeTrue())
		g.Expect(webhook.Webhooks).NotTo(BeEmpty())

		caBundle := webhook.Webhooks[0].ClientConfig.CABundle

		secret, err := framework.GetClients().KubeClient().CoreV1().Secrets(moduleNamespace).Get(context.TODO(), "operator-helm-controller-tls", metav1.GetOptions{})
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(secret.CreationTimestamp.After(deployAt.UTC().Add(-1 * time.Second))).To(BeTrue())

		caCert, found := secret.Data["ca.crt"]
		g.Expect(found).To(BeTrue())

		g.Expect(caBundle).To(Equal(caCert), "webhook caBundle does not match module ca certificate")
	}).WithTimeout(timeout).WithPolling(framework.PollingInterval).Should(Succeed())

	Eventually(func(g Gomega) {
		pods, err := framework.GetClients().KubeClient().CoreV1().
			Pods(moduleNamespace).
			List(context.Background(), metav1.ListOptions{})
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(pods.Items).NotTo(BeEmpty(),
			"no pods found in namespace %s", moduleNamespace)

		for _, pod := range pods.Items {
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
		Expect(err).NotTo(HaveOccurred(), "should remove deckhouse webhook-hander pods in d8-system namespace")
	}).WithTimeout(framework.ShortTimeout).WithPolling(framework.PollingInterval)

	UntilAllPodsReady("d8-system", "app=webhook-handler", 1, timeout)

	Consistently(func(g Gomega) {
		pods, err := framework.GetClients().KubeClient().CoreV1().
			Pods("d8-system").
			List(context.Background(), metav1.ListOptions{LabelSelector: "app=webhook-hander"})
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(len(pods.Items)).To(Equal(1),
			"expected %d pods, got %d", 1, len(pods.Items))

		for _, pod := range pods.Items {
			g.Expect(pod.Status.Phase).To(Equal(corev1.PodRunning),
				"pod %s phase: %s", pod.Name, pod.Status.Phase)
			for _, cs := range pod.Status.ContainerStatuses {
				g.Expect(cs.Ready).To(BeTrue(),
					"pod %s container %s not ready", pod.Name, cs.Name)
			}
		}
	}, "60s", "1s")
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
