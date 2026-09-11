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

package naming

import (
	"crypto/sha256"
	"fmt"
	"strings"
)

const (
	resourcePrefix = "tmp"

	// maxPartLen bounds each human-readable name part of a cluster-scoped name so
	// the whole name stays within the 63-character DNS-1123 label limit:
	// "tmp-" (4) + repo (<=20) + "-" + chart (<=20) + "-" + hash (16) = 62.
	maxPartLen = 20

	// maxNamespacedPartLen is the same bound for a namespaced name, which carries
	// one part more: "tmp-" (4) + namespace (<=12) + "-" + repo (<=12) + "-" +
	// chart (<=12) + "-" + hash (16) = 60.
	maxNamespacedPartLen = 12
)

// AuxResourceName returns a deterministic DNS-1123 name (<=63 chars) for the
// auxiliary source resource backing a (kind, namespace, repository, chart, version)
// tuple. The same tuple always maps to the same name, which makes polling requests
// idempotent and lets concurrent requests converge on one resource.
//
// namespace is empty for a cluster-scoped repository kind and is then absent from
// both the hash input and the readable part, so the names the addon family already
// uses do not move. For a namespaced kind it is what keeps two same-named
// repositories in different namespaces apart: the auxiliary objects of every kind
// share one namespace, so the tuple without it is not unique. A namespaced name
// carries one readable part more, so each of its parts is bounded more tightly.
//
// The readable parts are only hints; the hash over the full tuple guarantees
// uniqueness even if they collide.
func AuxResourceName(kind, namespace, repository, chart, version string) string {
	if namespace == "" {
		sum := sha256.Sum256([]byte(kind + "\x00" + repository + "/" + chart + "@" + version))

		return fmt.Sprintf("%s-%s-%s-%x", resourcePrefix, sanitize(repository, maxPartLen), sanitize(chart, maxPartLen), sum[:8])
	}

	sum := sha256.Sum256([]byte(kind + "\x00" + namespace + "/" + repository + "/" + chart + "@" + version))

	return fmt.Sprintf("%s-%s-%s-%s-%x",
		resourcePrefix,
		sanitize(namespace, maxNamespacedPartLen),
		sanitize(repository, maxNamespacedPartLen),
		sanitize(chart, maxNamespacedPartLen),
		sum[:8],
	)
}

// sanitize lowercases s, replaces characters invalid in a DNS-1123 label
// with '-', truncates to limit, and trims leading/trailing '-'.
func sanitize(s string, limit int) string {
	s = strings.ToLower(s)

	var b strings.Builder
	for _, r := range s {
		if b.Len() >= limit {
			break
		}
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteRune('-')
		}
	}

	return strings.Trim(b.String(), "-")
}
