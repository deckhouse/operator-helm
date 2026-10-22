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

package labels

import (
	"maps"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/client"

	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
)

// ApplyOwned brings the labels this controller owns on an object to the desired
// set and leaves every other label alone.
func ApplyOwned(obj client.Object, desired map[string]string) {
	labels := obj.GetLabels()
	if labels == nil {
		labels = make(map[string]string, len(desired))
	}

	for key := range labels {
		if _, wanted := desired[key]; wanted {
			continue
		}

		if controllerOwnsLabel(key) {
			delete(labels, key)
		}
	}

	maps.Copy(labels, desired)

	obj.SetLabels(labels)
}

func controllerOwnsLabel(key string) bool {
	// Ownership goes by domain: every label this controller sets is qualified with the API group.
	domain, _, qualified := strings.Cut(key, "/")

	return qualified && domain == helmv1alpha1.GroupName
}
