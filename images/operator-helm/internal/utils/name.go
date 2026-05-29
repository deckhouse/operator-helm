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
	"crypto/sha256"
	"fmt"
	"strings"
)

func GetHash(s string) string {
	h := sha256.New()
	h.Write([]byte(s))

	return fmt.Sprintf("%x", h.Sum(nil))[:12]
}

func GetInternalRepositoryAuthSecretName(internalRepoName string) string {
	prefix := "hcar-auth"

	hash := GetHash(fmt.Sprintf("%s-%s", prefix, internalRepoName))

	var result, postfix string

	result = prefix + "-"

	if len(internalRepoName) > 53 {
		result += internalRepoName[:40]
		postfix = "-" + hash
	} else {
		result += internalRepoName
	}

	return strings.TrimRight(result, "-") + postfix
}

func GetInternalRepositoryTLSSecretName(internalRepoName string) string {
	prefix := "hcar-tls"

	hash := GetHash(fmt.Sprintf("%s-%s", prefix, internalRepoName))

	var result, postfix string

	result = prefix + "-"

	if len(internalRepoName) > 54 {
		result += internalRepoName[:41]
		postfix = "-" + hash
	} else {
		result += internalRepoName
	}

	return strings.TrimRight(result, "-") + postfix
}

func GetHelmClusterAddonChartName(repoName, chartName string) string {
	hash := GetHash(fmt.Sprintf("%s-chart-%s", repoName, chartName))

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

func GetInternalHelmReleaseName(addonName string) string {
	prefix := "hca"
	hash := GetHash(fmt.Sprintf("%s-%s", prefix, addonName))

	result := prefix + "-"
	postfix := ""

	if len(addonName) > 59 {
		result += addonName[:46]
		postfix = "-" + hash
	} else {
		result += addonName
	}

	return strings.TrimRight(result, "-") + postfix
}

func GetInternalHelmChartName(addonName string) string {
	return GetInternalHelmReleaseName(addonName)
}

func GetInternalOCIRepositoryName(addonName string) string {
	prefix := "hca"
	hash := GetHash(fmt.Sprintf("%s-%s", prefix, addonName))

	result := prefix + "-"
	postfix := ""

	if len(addonName) > 59 {
		result += addonName[:46]
		postfix = "-" + hash
	} else {
		result += addonName
	}

	return strings.TrimRight(result, "-") + postfix
}

func GetInternalHelmRepositoryName(addonRepositoryName string) string {
	prefix := "hcar"
	hash := GetHash(fmt.Sprintf("%s-%s", prefix, addonRepositoryName))

	result := prefix + "-"
	postfix := ""

	if len(addonRepositoryName) > 58 {
		result += addonRepositoryName[:45]
		postfix = "-" + hash
	} else {
		result += addonRepositoryName
	}

	return strings.TrimRight(result, "-") + postfix
}
