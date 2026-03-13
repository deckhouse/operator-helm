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
	"fmt"
	"hash/fnv"
	"strings"
)

func GetHash(s string) string {
	h := fnv.New32a()

	_, _ = h.Write([]byte(s))

	return fmt.Sprintf("%x", h.Sum32())
}

func GetInternalRepositoryAuthSecretName(repoType InternalRepositoryType, internalRepoName string) string {
	prefix := "auth"

	hash := GetHash(fmt.Sprintf("%s-%s-%s", prefix, repoType, internalRepoName))

	var result, postfix string

	result = prefix + "-" + string(repoType) + "-"

	if len(internalRepoName) > 35 {
		result += internalRepoName[:35]
		postfix = "-" + hash
	} else {
		result += internalRepoName
	}

	return strings.TrimRight(result, "-") + postfix
}

func GetInternalRepositoryTLSSecretName(repoType InternalRepositoryType, internalRepoName string) string {
	prefix := "tls"

	hash := GetHash(fmt.Sprintf("%s-%s-%s", prefix, repoType, internalRepoName))

	var result, postfix string

	result = prefix + "-" + string(repoType) + "-"

	if len(internalRepoName) > 35 {
		result += internalRepoName[:35]
		postfix = "-" + hash
	} else {
		result += internalRepoName
	}

	return strings.TrimRight(result, "-") + postfix
}

func GetHelmClusterAddonChartName(repoName, addonName string) string {
	hash := GetHash(fmt.Sprintf("%s-%s", repoName, addonName))

	var result, postfix string

	if len(repoName) > 20 {
		result += repoName[:20]
		postfix = "-" + hash
	} else {
		result += repoName
	}

	if len(addonName) > 20 {
		result += "-" + addonName[:20]
		postfix = "-" + hash
	} else {
		result += "-" + addonName
	}

	return strings.TrimRight(result, "-") + postfix
}

func GetInternalHelmReleaseName(addonName string) string {
	prefix := "addon"
	hash := GetHash(fmt.Sprintf("%s-%s", prefix, addonName))

	result := prefix + "-"
	postfix := ""

	if len(addonName) > 40 {
		result += addonName[:40]
		postfix = "-" + hash
	} else {
		result += addonName
	}

	return strings.TrimRight(result, "-") + postfix
}

func GetInternalHelmChartName(addonName string) string {
	return GetInternalHelmReleaseName(addonName)
}
