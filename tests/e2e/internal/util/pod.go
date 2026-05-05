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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/deckhouse/operator-helm/tests/e2e/internal/framework"
)

// UntilControllerReady waits for all controller pods to be Running with all
// containers Ready and zero restarts.
func UntilControllerReady(namespace, labelSelector string, timeout time.Duration) {
	GinkgoHelper()
	Eventually(func(g Gomega) {
		pods, err := framework.GetClients().KubeClient().CoreV1().
			Pods(namespace).
			List(context.Background(), metav1.ListOptions{LabelSelector: labelSelector})
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(pods.Items).NotTo(BeEmpty(),
			"no controller pods found with selector %s in namespace %s", labelSelector, namespace)

		for _, pod := range pods.Items {
			g.Expect(pod.Status.Phase).To(Equal(corev1.PodRunning),
				"pod %s is %s, not Running", pod.Name, pod.Status.Phase)

			for _, cs := range pod.Status.ContainerStatuses {
				g.Expect(cs.Ready).To(BeTrue(),
					"container %s in pod %s is not ready", cs.Name, pod.Name)
				g.Expect(cs.RestartCount).To(BeZero(),
					"container %s in pod %s has %d restarts", cs.Name, pod.Name, cs.RestartCount)
			}
		}
	}).WithTimeout(timeout).WithPolling(framework.PollingInterval).Should(Succeed())
}

// AssertPodsExist verifies that at least minCount pods matching the selector
// exist in the namespace.
func AssertPodsExist(namespace, labelSelector string, minCount int) {
	GinkgoHelper()
	pods, err := framework.GetClients().KubeClient().CoreV1().
		Pods(namespace).
		List(context.Background(), metav1.ListOptions{LabelSelector: labelSelector})
	Expect(err).NotTo(HaveOccurred())
	Expect(len(pods.Items)).To(BeNumerically(">=", minCount),
		"expected >= %d pods in %s with selector %s, got %d",
		minCount, namespace, labelSelector, len(pods.Items))
}

// UntilPodsExist waits until at least minCount pods appear.
func UntilPodsExist(namespace, labelSelector string, minCount int, timeout time.Duration) {
	GinkgoHelper()
	Eventually(func(g Gomega) {
		pods, err := framework.GetClients().KubeClient().CoreV1().
			Pods(namespace).
			List(context.Background(), metav1.ListOptions{LabelSelector: labelSelector})
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(len(pods.Items)).To(BeNumerically(">=", minCount),
			"waiting for >= %d pods, got %d", minCount, len(pods.Items))
	}).WithTimeout(timeout).WithPolling(framework.PollingInterval).Should(Succeed())
}

// UntilPodCount waits for exactly expectedCount Running pods (excluding
// pods being deleted).
func UntilPodCount(namespace, labelSelector string, expectedCount int, timeout time.Duration) {
	GinkgoHelper()
	Eventually(func(g Gomega) {
		pods, err := framework.GetClients().KubeClient().CoreV1().
			Pods(namespace).
			List(context.Background(), metav1.ListOptions{LabelSelector: labelSelector})
		g.Expect(err).NotTo(HaveOccurred())

		runningCount := 0
		for _, pod := range pods.Items {
			if pod.Status.Phase == corev1.PodRunning && pod.DeletionTimestamp == nil {
				runningCount++
			}
		}

		g.Expect(runningCount).To(Equal(expectedCount),
			"expected %d running pods, got %d (total listed: %d)",
			expectedCount, runningCount, len(pods.Items))
	}).WithTimeout(timeout).WithPolling(framework.PollingInterval).Should(Succeed())
}

// UntilAllPodsReady waits for exactly expectedCount pods to be Running and
// all their containers Ready.
func UntilAllPodsReady(namespace, labelSelector string, expectedCount int, timeout time.Duration) {
	GinkgoHelper()
	Eventually(func(g Gomega) {
		pods, err := framework.GetClients().KubeClient().CoreV1().
			Pods(namespace).
			List(context.Background(), metav1.ListOptions{LabelSelector: labelSelector})
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(len(pods.Items)).To(Equal(expectedCount),
			"expected %d pods, got %d", expectedCount, len(pods.Items))

		for _, pod := range pods.Items {
			g.Expect(pod.Status.Phase).To(Equal(corev1.PodRunning),
				"pod %s phase: %s", pod.Name, pod.Status.Phase)
			for _, cs := range pod.Status.ContainerStatuses {
				g.Expect(cs.Ready).To(BeTrue(),
					"pod %s container %s not ready", pod.Name, cs.Name)
			}
		}
	}).WithTimeout(timeout).WithPolling(framework.PollingInterval).Should(Succeed())
}
