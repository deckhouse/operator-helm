package framework

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func (f *Framework) saveDump() {
	testName := sanitizeTestName(CurrentSpecReport().FullText())
	dir := getTmpDir()

	f.dumpNamespaceResources(testName, dir)
	f.dumpAllControllerLogs(testName, dir)
}

func (f *Framework) dumpNamespaceResources(testName, dir string) {
	if f.namespace == nil {
		return
	}

	type gvr struct {
		group, version, resource string
	}
	resources := []gvr{
		{"", "v1", "pods"},
		{"", "v1", "services"},
		{"", "v1", "configmaps"},
		{"", "v1", "events"},
		{"apps", "v1", "deployments"},
	}
	for _, r := range resources {
		list, err := f.dynamic.Resource(
			schema.GroupVersionResource{Group: r.group, Version: r.version, Resource: r.resource},
		).Namespace(f.namespace.Name).List(context.Background(), metav1.ListOptions{})
		if err != nil {
			GinkgoWriter.Printf("Failed to list %s in namespace %s: %v\n", r.resource, f.namespace.Name, err)
			continue
		}
		if len(list.Items) == 0 {
			continue
		}

		fileName := fmt.Sprintf("%s/e2e_failed__%s__%s.yaml", dir, testName, r.resource)
		data, err := list.MarshalJSON()
		if err != nil {
			GinkgoWriter.Printf("Failed to marshal %s: %v\n", r.resource, err)
			continue
		}
		if err := os.WriteFile(fileName, data, 0o644); err != nil {
			GinkgoWriter.Printf("Failed to write %s dump: %v\n", r.resource, err)
		}
	}
}

func (f *Framework) dumpAllControllerLogs(testName, dir string) {
	cfg := GetConfig()
	for _, ctrl := range cfg.Controllers {
		pods, err := f.kubeClient.CoreV1().
			Pods(ctrl.Namespace).
			List(context.Background(), metav1.ListOptions{LabelSelector: ctrl.LabelSelector})
		if err != nil {
			GinkgoWriter.Printf("WARNING: cannot list pods for %s: %v\n", ctrl.Name, err)
			continue
		}

		for _, pod := range pods.Items {
			containers := ctrl.Containers
			if len(containers) == 0 {
				for _, c := range pod.Spec.Containers {
					containers = append(containers, c.Name)
				}
			}

			for _, container := range containers {
				f.dumpContainerLogs(testName, dir, ctrl.Name, pod.Name, pod.Namespace, container)
			}
		}
	}
}

func (f *Framework) dumpContainerLogs(testName, dir, controllerName, podName, namespace, container string) {
	stream, err := f.kubeClient.CoreV1().
		Pods(namespace).
		GetLogs(podName, &corev1.PodLogOptions{Container: container}).
		Stream(context.Background())
	if err != nil {
		GinkgoWriter.Printf("Failed to get logs for %s/%s/%s: %v\n", controllerName, podName, container, err)
		return
	}
	defer stream.Close()

	data, err := io.ReadAll(stream)
	if err != nil {
		GinkgoWriter.Printf("Failed to read logs for %s/%s/%s: %v\n", controllerName, podName, container, err)
		return
	}

	fileName := fmt.Sprintf("%s/e2e_failed__%s__%s__%s__%s.log", dir, testName, controllerName, podName, container)
	if err := os.WriteFile(fileName, data, 0o644); err != nil {
		GinkgoWriter.Printf("Failed to save logs for %s/%s/%s: %v\n", controllerName, podName, container, err)
	}
}

func sanitizeTestName(name string) string {
	r := strings.NewReplacer(
		" ", "_", ":", "_", "[", "_", "]", "_",
		"(", "_", ")", "_", "|", "_", "`", "", "'", "",
	)
	return r.Replace(strings.ToLower(name))
}

func getTmpDir() string {
	if dir := os.Getenv("RUNNER_TEMP"); dir != "" {
		return dir
	}
	return "/tmp"
}
