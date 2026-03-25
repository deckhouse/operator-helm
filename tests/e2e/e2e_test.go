package e2e_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/deckhouse/operator-helm/tests/e2e/internal/controller"

	_ "github.com/deckhouse/operator-helm/tests/e2e/helmclusteraddon"
)

func TestE2E(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "E2E Suite")
}

var _ = SynchronizedBeforeSuite(func() {
	controller.StartAll()
	controller.SaveRestartCounts()
}, func() {})

var _ = SynchronizedAfterSuite(func() {}, func() {
	controller.StopAll()
	controller.AssertNoErrors()
	controller.AssertNoRestarts()
})
