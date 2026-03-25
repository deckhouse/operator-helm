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

package controller

import (
	"context"
	"fmt"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/deckhouse/operator-helm/tests/e2e/internal/framework"
)

type restartSnapshot struct {
	Controller string
	Pod        string
	Container  string
	Count      int32
}

var initialRestarts []restartSnapshot

// SaveRestartCounts records the current restart count for all controller containers.
func SaveRestartCounts() {
	cfg := framework.GetConfig()
	initialRestarts = nil

	for _, ctrl := range cfg.Controllers {
		pods, err := framework.GetClients().KubeClient().CoreV1().
			Pods(ctrl.Namespace).
			List(context.Background(), metav1.ListOptions{LabelSelector: ctrl.LabelSelector})
		if err != nil {
			GinkgoWriter.Printf("WARNING: cannot list pods for controller %q: %v\n", ctrl.Name, err)
			continue
		}

		for _, pod := range pods.Items {
			for _, cs := range pod.Status.ContainerStatuses {
				initialRestarts = append(initialRestarts, restartSnapshot{
					Controller: ctrl.Name,
					Pod:        pod.Name,
					Container:  cs.Name,
					Count:      cs.RestartCount,
				})
			}
		}
	}
}

// AssertNoRestarts fails the test if any controller container restarted during the suite.
func AssertNoRestarts() {
	GinkgoHelper()
	cfg := framework.GetConfig()

	var restartMessages []string

	for _, ctrl := range cfg.Controllers {
		pods, err := framework.GetClients().KubeClient().CoreV1().
			Pods(ctrl.Namespace).
			List(context.Background(), metav1.ListOptions{LabelSelector: ctrl.LabelSelector})
		Expect(err).NotTo(HaveOccurred(), "failed to list pods for controller %q", ctrl.Name)

		for _, pod := range pods.Items {
			for _, cs := range pod.Status.ContainerStatuses {
				initial := findInitialCount(ctrl.Name, pod.Name, cs.Name)
				if cs.RestartCount > initial {
					restartMessages = append(restartMessages, fmt.Sprintf(
						"controller %q pod %s container %s: restarts before=%d after=%d",
						ctrl.Name, pod.Name, cs.Name, initial, cs.RestartCount,
					))
				}
			}
		}
	}

	if len(restartMessages) > 0 {
		Fail(fmt.Sprintf("Controller restarts detected:\n  %s", strings.Join(restartMessages, "\n  ")))
	}
}

func findInitialCount(controllerName, pod, container string) int32 {
	for _, s := range initialRestarts {
		if s.Controller == controllerName && s.Pod == pod && s.Container == container {
			return s.Count
		}
	}
	return 0
}
