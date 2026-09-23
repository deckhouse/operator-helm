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
// Deckhouse rather than to a user: the three namespaces Kubernetes creates for
// itself, and every d8- one.
//
// The kube- prefix as a whole is deliberately not rejected, however reserved it is.
// This predicate also decides where an addon may already be installed, and turning
// it into a prefix test would move an existing addon in, say, kube-prometheus from
// reconciling to permanently failed — with no way back, since the webhook refuses
// every edit to an object whose target namespace it rejects.
func IsSystemNamespace(namespace string) bool {
	switch namespace {
	case "kube-system", "kube-node-lease", "kube-public":
		return true
	}

	return strings.HasPrefix(namespace, "d8-")
}
