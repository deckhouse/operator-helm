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
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/deckhouse/operator-helm/tests/e2e/internal/framework"
)

var operatorHelmInternalHelmRepositoryGVR = schema.GroupVersionResource{
	Group:    "source.internal.operator-helm.deckhouse.io",
	Version:  "v1",
	Resource: "internalnelmoperatorhelmrepositories",
}

var operatorHelmInternalOCIRepositoryGVR = schema.GroupVersionResource{
	Group:    "source.internal.operator-helm.deckhouse.io",
	Version:  "v1",
	Resource: "internalnelmoperatorocirepositories",
}

func DeleteHelmClusterAddonRepository(f *framework.Framework, name, repositoryType string, timeout time.Duration) {
	GinkgoHelper()

	err := f.OperatorClient().HelmV1alpha1().HelmClusterAddonRepositories().Delete(context.TODO(), name, metav1.DeleteOptions{})
	Expect(client.IgnoreNotFound(err)).NotTo(HaveOccurred(), "failed to remove HelmClusterAddonRepository")

	UntilHelmClusterAddonRepositoryDeleted(name, repositoryType, timeout)
}

func UntilHelmClusterAddonRepositoryDeleted(name, repositoryType string, timeout time.Duration) {
	repositoryType = strings.ToLower(repositoryType)
	repositoryName := "hcar-" + name

	Eventually(func(g Gomega) {
		var err error

		switch repositoryType {
		case "helm":
			_, err = framework.GetClients().DynamicClient().Resource(operatorHelmInternalHelmRepositoryGVR).Namespace(moduleNamespace).Get(context.TODO(), repositoryName, metav1.GetOptions{})
		case "oci":
			_, err = framework.GetClients().DynamicClient().Resource(operatorHelmInternalOCIRepositoryGVR).Namespace(moduleNamespace).Get(context.TODO(), repositoryName, metav1.GetOptions{})
		default:
			panic("unsupporeted repositoryType")
		}
		g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "Internal repository should be deleted")
	}).WithTimeout(timeout).WithPolling(framework.PollingInterval).Should(Succeed())
}
