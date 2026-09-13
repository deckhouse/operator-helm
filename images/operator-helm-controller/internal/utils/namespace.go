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

package utils

import (
	"strings"
)

// IsSystemNamespace reports whether a namespace belongs to the cluster or to
// Deckhouse rather than to a user. The set matches what Deckhouse itself excludes
// from a user's reach: every kube- namespace, every d8- one, and default, which is
// shared by everyone and owned by no one.
func IsSystemNamespace(namespace string) bool {
	if namespace == "default" {
		return true
	}

	return strings.HasPrefix(namespace, "kube-") || strings.HasPrefix(namespace, "d8-")
}
