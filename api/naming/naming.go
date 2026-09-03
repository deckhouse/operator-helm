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
// that mirrors one chart of a repository. It lives in the api module because
// operator-helm-controller writes those objects while chart-values-controller reads
// them: the name is a truncated hash, so both must derive it identically.
func HelmClusterAddonChartName(repoName, chartName string) string {
	hash := hash(fmt.Sprintf("%s-chart-%s", repoName, chartName))

	var result, postfix string

	if len(repoName) > 20 {
		result += repoName[:20] + "-chart-"
		postfix = "-" + hash
	} else {
		result += repoName + "-chart-"
	}

	if len(chartName) > 20 {
		result += chartName[:20]
		postfix = "-" + hash
	} else {
		result += chartName
	}

	return strings.TrimRight(result, "-") + postfix
}

func hash(s string) string {
	h := sha256.New()
	h.Write([]byte(s))

	return fmt.Sprintf("%x", h.Sum(nil))[:12]
}
