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

// chartObjectName is the single naming scheme behind every chart catalog kind. It
// lives in the api module because operator-helm-controller writes those objects
// while chart-values-controller reads them: the name is a truncated hash, so both
// must derive it identically. Names coincide across families on purpose — the
// objects differ in kind, and the namespaced and cluster variants live in
// different scopes, so a shared name cannot collide.
func chartObjectName(repoName, chartName string) string {
	hash := hash(fmt.Sprintf("%s-chart-%s", repoName, chartName))

	var result, postfix string

	if len(repoName) > 20 {
		// The truncated part is followed by a separator, so a dash or a dot the
		// cut left behind has to go here: the final trim only reaches the end of
		// the whole name.
		result += strings.TrimRight(repoName[:20], "-.") + "-chart-"
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

	return strings.TrimRight(result, "-.") + postfix
}

func hash(s string) string {
	h := sha256.New()
	h.Write([]byte(s))

	return fmt.Sprintf("%x", h.Sum(nil))[:12]
}
