/*
Copyright 2026 Flant JSC

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

package kube

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

var (
	deploymentGVR = schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
	podGVR        = schema.GroupVersionResource{Group: "", Version: "v1", Resource: "pods"}
)

// NamespaceHasWorkloads reports whether the namespace still contains any Deployment or Pod.
// It is used during module teardown to make sure the controllers are gone before their
// resources are touched.
func NamespaceHasWorkloads(ctx context.Context, dynamicClient dynamic.Interface, namespace string) (bool, error) {
	for _, gvr := range []schema.GroupVersionResource{deploymentGVR, podGVR} {
		list, err := dynamicClient.Resource(gvr).Namespace(namespace).List(ctx, metav1.ListOptions{Limit: 1})
		if err != nil {
			return false, fmt.Errorf("listing %s in namespace %s: %w", gvr.Resource, namespace, err)
		}

		if len(list.Items) > 0 {
			return true, nil
		}
	}

	return false, nil
}
