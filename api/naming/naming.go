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

// HelmClusterAddonChartName derives the name of the HelmClusterAddonChart object
// that mirrors one chart of a repository.
func HelmClusterAddonChartName(repoName, chartName string) string {
	return chartObjectName(repoName, chartName)
}

// ApplicationChartName derives the name of the HelmApplicationChart object that
// mirrors one chart of a HelmApplicationRepository. The object is namespaced, so
// the name only has to be unique inside the repository's namespace.
func ApplicationChartName(repoName, chartName string) string {
	return chartObjectName(repoName, chartName)
}

// ClusterApplicationChartName derives the name of the HelmClusterApplicationChart
// object that mirrors one chart of a HelmClusterApplicationRepository.
func ClusterApplicationChartName(repoName, chartName string) string {
	return chartObjectName(repoName, chartName)
}

// chartObjectName is the naming scheme behind every chart catalog kind. It lives in
// the api module because operator-helm-controller writes those objects while
// chart-values-controller reads them, so both must derive the name identically.
//
// Joining the two parts with a separator that may itself appear inside them is not
// injective: "abc" + "def-chart-ghi" and "abc-chart-def" + "ghi" produce the same
// readable part, and the two repositories then fight over one catalog object. The
// hash is what separates them, so every family always carries it — and it is taken
// over the two parts joined by a byte no object name can hold, because hashing the
// readable join would reproduce the very ambiguity it is there to resolve. The hash
// is taken over the raw inputs, not the sanitized readable parts below, so it stays
// injective even when sanitizing two different inputs happens to yield the same
// readable part.
func chartObjectName(repoName, chartName string) string {
	hash := hash(repoName + "\x00" + chartName)

	repoPart := sanitize(repoName)
	chartPart := sanitize(chartName)

	var result string

	if len(repoPart) > 20 {
		// The truncated part is followed by a separator, so a dash or a dot the
		// cut left behind has to go here: the final trim only reaches the end of
		// the whole name.
		result += strings.TrimRight(repoPart[:20], "-.") + "-chart-"
	} else {
		// Same reasoning as the truncated branch above: repoPart is followed by
		// a separator here too, so a trailing dash or dot has to be trimmed
		// before it, not left for the final trim to reach.
		result += strings.TrimRight(repoPart, "-.") + "-chart-"
	}

	if len(chartPart) > 20 {
		result += chartPart[:20]
	} else {
		result += chartPart
	}

	// A repoPart that sanitizes to empty (or to only separators) leaves the fixed
	// "-chart-" literal leading the name, so the trim has to reach the front too.
	readable := []byte(strings.Trim(result, "-."))

	// Trimming the ends is not enough. sanitize keeps dots, and a dot is only legal
	// between two alphanumerics, so an input like "my chart. v2" leaves ".-" inside
	// the readable part and the whole name stops being a subdomain the API server
	// accepts. Every dot the label rules cannot hold becomes a dash instead; the
	// readable part is only a hint, and the hash keeps the name injective whatever
	// this does to it. The bounds hold because the trim above already removed every
	// leading and trailing separator.
	for i := 1; i < len(readable)-1; i++ {
		if readable[i] == '.' && (!isAlphanumeric(readable[i-1]) || !isAlphanumeric(readable[i+1])) {
			readable[i] = '-'
		}
	}

	return string(readable) + "-" + hash
}

func isAlphanumeric(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')
}

// sanitize lower-cases s and replaces every character that cannot appear in a
// DNS-1123 subdomain (anything outside [a-z0-9.-]) with a dash, so a chart or
// repository name coming from an index or an OCI tag — "MyChart", "ch art" — always
// contributes a valid object name segment. It does not trim or truncate: that is
// left to the caller, which needs to do both around the fixed "-chart-" separator.
func sanitize(s string) string {
	s = strings.ToLower(s)

	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '.' || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteRune('-')
		}
	}

	return b.String()
}

func hash(s string) string {
	h := sha256.New()
	h.Write([]byte(s))

	return fmt.Sprintf("%x", h.Sum(nil))[:12]
}
