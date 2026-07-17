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

package e2e_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	_ "github.com/deckhouse/operator-helm/tests/e2e/helmclusteraddon"
	_ "github.com/deckhouse/operator-helm/tests/e2e/helmclusteraddonrepository"
	"github.com/deckhouse/operator-helm/tests/e2e/internal/controller"
	"github.com/deckhouse/operator-helm/tests/e2e/internal/framework"
	"github.com/deckhouse/operator-helm/tests/e2e/internal/util"
)

func TestE2E(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "E2E Suite")
}

// The module is enabled once for the whole suite (it is the most expensive part
// of the run) and always disabled at teardown. Enabling before StartAll/
// SaveRestartCounts ensures the log watchers and restart baseline see the running
// controller pods.
//
// Teardown is done via DeferCleanup so it always runs, even if setup, specs, or
// the assertions fail. DeferCleanup runs its closures in reverse registration
// order and runs all of them regardless of failures, so the module disable is
// registered first (runs last) and the assertions last (run first, while the
// controller pods are still up, before the module is torn down).
var _ = SynchronizedBeforeSuite(func() {
	f := framework.NewFramework("")
	cfg := framework.GetConfig()

	DeferCleanup(func() {
		if framework.IsCleanUpNeeded() {
			By("Disabling operator-helm module")
			util.DisableModuleConfig(framework.LongTimeout)
			util.UntilModuleDisabled(framework.LongTimeout)
		}
	})

	deployAt := metav1.Now()

	By("Enabling operator-helm module")
	util.EnsureModuleConfig(f)
	util.UntilModuleEnabled(deployAt, framework.LongTimeout)

	By("Waiting for all controllers to be ready")
	for _, ctrl := range cfg.Controllers {
		util.UntilControllerReady(ctrl.Namespace, ctrl.LabelSelector, framework.LongTimeout)
	}

	controller.StartAll()
	controller.SaveRestartCounts()

	DeferCleanup(func() {
		controller.StopAll()
		controller.AssertNoErrors()
		controller.AssertNoRestarts()
	})
}, func() {})
