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
	"bufio"
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/deckhouse/operator-helm/tests/e2e/internal/framework"
)

// LogError stores a single error line found in controller logs.
type LogError struct {
	Controller string
	Pod        string
	Container  string
	Line       string
	Timestamp  time.Time
}

func (e LogError) String() string {
	return fmt.Sprintf("[%s] %s/%s: %s", e.Controller, e.Pod, e.Container, e.Line)
}

// controllerWatcher monitors logs for a single controller.
type controllerWatcher struct {
	config         framework.ControllerConfig
	errors         []LogError
	mu             sync.Mutex
	cancel         context.CancelFunc
	excludeStrings []string
}

// LogWatchManager manages watchers for all controllers.
type LogWatchManager struct {
	watchers map[string]*controllerWatcher
}

var manager *LogWatchManager

// StartAll begins streaming logs for all controllers defined in config.
func StartAll() {
	cfg := framework.GetConfig()
	manager = &LogWatchManager{
		watchers: make(map[string]*controllerWatcher, len(cfg.Controllers)),
	}

	for _, ctrlCfg := range cfg.Controllers {
		w := newControllerWatcher(ctrlCfg)
		manager.watchers[ctrlCfg.Name] = w
		w.start()
	}
}

// StopAll stops all log watchers.
func StopAll() {
	if manager == nil {
		return
	}
	for _, w := range manager.watchers {
		w.stop()
	}
}

// AssertNoErrors fails the test if any controller logged errors.
func AssertNoErrors() {
	GinkgoHelper()
	if manager == nil {
		return
	}

	var allErrors []LogError
	for _, w := range manager.watchers {
		allErrors = append(allErrors, w.getErrors()...)
	}

	if len(allErrors) > 0 {
		var sb strings.Builder
		sb.WriteString(fmt.Sprintf("Found %d error(s) in controller logs:\n\n", len(allErrors)))
		for i, e := range allErrors {
			sb.WriteString(fmt.Sprintf("  %d. %s\n", i+1, e.String()))
		}
		Fail(sb.String())
	}
}

// GetErrors returns collected errors for a specific controller.
func GetErrors(controllerName string) []LogError {
	if manager == nil {
		return nil
	}
	w, ok := manager.watchers[controllerName]
	if !ok {
		return nil
	}
	return w.getErrors()
}

// AssertNoErrorsFor fails the test if the named controller has errors.
func AssertNoErrorsFor(controllerName string) {
	GinkgoHelper()
	errs := GetErrors(controllerName)
	Expect(errs).To(BeEmpty(),
		"controller %q has %d error(s) in logs:\n%v", controllerName, len(errs), errs)
}

func newControllerWatcher(cfg framework.ControllerConfig) *controllerWatcher {
	return &controllerWatcher{
		config:         cfg,
		excludeStrings: cfg.LogFilters.Exclude,
	}
}

func (w *controllerWatcher) start() {
	ctx, cancel := context.WithCancel(context.Background())
	w.cancel = cancel

	pods, err := framework.GetClients().KubeClient().CoreV1().
		Pods(w.config.Namespace).
		List(ctx, metav1.ListOptions{LabelSelector: w.config.LabelSelector})
	if err != nil {
		GinkgoWriter.Printf("WARNING: cannot list pods for controller %q: %v\n", w.config.Name, err)
		return
	}

	for _, pod := range pods.Items {
		containers := w.config.Containers
		if len(containers) == 0 {
			for _, c := range pod.Spec.Containers {
				containers = append(containers, c.Name)
			}
		}

		for _, container := range containers {
			go w.streamLogs(ctx, pod.Name, container)
		}
	}
}

func (w *controllerWatcher) stop() {
	if w.cancel != nil {
		w.cancel()
	}
}

func (w *controllerWatcher) streamLogs(ctx context.Context, podName, containerName string) {
	sinceTime := metav1.Now()
	stream, err := framework.GetClients().KubeClient().CoreV1().
		Pods(w.config.Namespace).
		GetLogs(podName, &corev1.PodLogOptions{
			Container: containerName,
			Follow:    true,
			SinceTime: &sinceTime,
		}).Stream(ctx)
	if err != nil {
		GinkgoWriter.Printf("WARNING: cannot stream logs for %s/%s/%s: %v\n",
			w.config.Name, podName, containerName, err)
		return
	}
	defer stream.Close()

	scanner := bufio.NewScanner(stream)
	for scanner.Scan() {
		line := scanner.Text()
		if w.isError(line) && !w.isExcluded(line) {
			w.addError(LogError{
				Controller: w.config.Name,
				Pod:        podName,
				Container:  containerName,
				Line:       line,
				Timestamp:  time.Now(),
			})
		}
	}
}

func (w *controllerWatcher) isError(line string) bool {
	lower := strings.ToLower(line)

	if strings.Contains(lower, `"level":"error"`) ||
		strings.Contains(lower, `"level":"fatal"`) ||
		strings.Contains(lower, `"level":"dpanic"`) {
		return true
	}

	// klog format: E0324 10:00:00.000000 ...
	if len(line) > 1 && line[0] == 'E' && line[1] >= '0' && line[1] <= '9' {
		return true
	}

	if strings.Contains(lower, "level=error") ||
		strings.Contains(lower, "panic:") {
		return true
	}

	return false
}

func (w *controllerWatcher) isExcluded(line string) bool {
	for _, s := range w.excludeStrings {
		if strings.Contains(line, s) {
			return true
		}
	}
	for _, re := range w.config.CompiledRegexps() {
		if re.MatchString(line) {
			return true
		}
	}
	return false
}

func (w *controllerWatcher) addError(err LogError) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.errors = append(w.errors, err)
}

func (w *controllerWatcher) getErrors() []LogError {
	w.mu.Lock()
	defer w.mu.Unlock()
	copied := make([]LogError, len(w.errors))
	copy(copied, w.errors)
	return copied
}
