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

	// maxPartLen bounds each human-readable name part so the whole name stays
	// within the 63-character DNS-1123 label limit:
	// "tmp-" (4) + repo (<=20) + "-" + chart (<=20) + "-" + hash (16) = 62.
	maxPartLen = 20
)

// AuxResourceName returns a deterministic DNS-1123 name (<=63 chars) for the
// auxiliary source resource backing a (kind, repository, chart, version) tuple:
// "tmp-<repo>-<chart>-<hash>". The same tuple always maps to the same name,
// which makes polling requests idempotent and lets concurrent requests converge
// on one resource. The repo/chart parts are only human-readable hints; the hash
// over the full tuple (including kind) guarantees uniqueness even if those parts
// collide across repository kinds.
func AuxResourceName(kind, repository, chart, version string) string {
	sum := sha256.Sum256([]byte(kind + "\x00" + repository + "/" + chart + "@" + version))

	return fmt.Sprintf("%s-%s-%s-%x", resourcePrefix, sanitizePart(repository), sanitizePart(chart), sum[:8])
}

// sanitizePart lowercases s, replaces characters invalid in a DNS-1123 label
// with '-', truncates to maxPartLen, and trims leading/trailing '-'.
func sanitizePart(s string) string {
	s = strings.ToLower(s)

	var b strings.Builder
	for _, r := range s {
		if b.Len() >= maxPartLen {
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
