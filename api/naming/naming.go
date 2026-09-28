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
// mirrors one chart of a HelmApplicationRepository.
func ApplicationChartName(repoName, chartName string) string {
	return chartObjectName(repoName, chartName)
}

// ClusterApplicationChartName derives the name of the HelmClusterApplicationChart
// object that mirrors one chart of a HelmClusterApplicationRepository.
func ClusterApplicationChartName(repoName, chartName string) string {
	return chartObjectName(repoName, chartName)
}

func chartObjectName(repoName, chartName string) string {
	// The hash covers the raw inputs joined by "\x00", not the readable part below.
	hash := hash(repoName + "\x00" + chartName)

	repoPart := sanitize(repoName)
	chartPart := sanitize(chartName)

	var result string

	if len(repoPart) > 20 {
		result += strings.TrimRight(repoPart[:20], "-.") + "-chart-"
	} else {
		result += strings.TrimRight(repoPart, "-.") + "-chart-"
	}

	if len(chartPart) > 20 {
		result += chartPart[:20]
	} else {
		result += chartPart
	}

	readable := []byte(strings.Trim(result, "-."))

	// A dot is legal only between two alphanumerics; every other dot becomes a dash.
	for i := 1; i < len(readable)-1; i++ {
		if readable[i] == '.' && (!isAlphanumeric(readable[i-1]) || !isAlphanumeric(readable[i+1])) {
			readable[i] = '-'
		}
	}

	return collapseDashes(string(readable)) + "-" + hash
}

func collapseDashes(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '-' && i > 0 && s[i-1] == '-' {
			continue
		}

		b.WriteByte(s[i])
	}

	return b.String()
}

func isAlphanumeric(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')
}

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
