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

package helmclusteraddonrepository

import (
	"fmt"
	"hash/fnv"
	"strings"
)

func GenerateSafeName(baseName string, maxLen int) string {
	if len(baseName) <= maxLen {
		return baseName
	}

	h := fnv.New32a()
	h.Write([]byte(baseName))
	hashStr := fmt.Sprintf("%x", h.Sum32())[:5]

	truncated := baseName[:maxLen-6]
	truncated = strings.TrimRight(truncated, "-")

	return fmt.Sprintf("%s-%s", truncated, hashStr)
}

func GetRepositoryAuthSecretName(repoType InternalRepositoryType, internalRepoName string) string {
	return GenerateSafeName("auth-"+string(repoType)+"-"+internalRepoName, 63)
}

func GetRepositoryTLSSecretName(repoType InternalRepositoryType, internalRepoName string) string {
	return GenerateSafeName("tls-"+string(repoType)+"-"+internalRepoName, 63)
}

func GetRepositoryType(scheme string) (InternalRepositoryType, error) {
	switch scheme {
	case "http", "https":
		return InternalHelmRepository, nil
	case "oci":
		return InternalOCIRepository, nil
	default:
		return "", fmt.Errorf("unsupported repository schema in use: %s", scheme)
	}
}

func GetHelmClusterAddonChartName(repoName, chartName string) string {
	return GenerateSafeName(repoName+"-"+chartName, 63)
}
