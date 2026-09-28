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

const hashLength = 12

func GetHash(s string) string {
	h := sha256.New()
	h.Write([]byte(s))

	return fmt.Sprintf("%x", h.Sum(nil))[:hashLength]
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

	return strings.TrimRight(result, "-.") + postfix
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

	return strings.TrimRight(result, "-.") + postfix
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

	return strings.TrimRight(result, "-.") + postfix
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

	return strings.TrimRight(result, "-.") + postfix
}

// GetChartClaimLeaseName derives the name of the Lease that guards uniqueness of a
// repository/chart pair across HelmClusterAddon objects. The name is a pure hash of the pair.
func GetChartClaimLeaseName(repoName, chartName string) string {
	prefix := "hca-claim"
	hash := GetHash(fmt.Sprintf("%s-%s-%s", prefix, repoName, chartName))

	return prefix + "-" + hash
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

	return strings.TrimRight(result, "-.") + postfix
}

const derivedPartLimit = 18 // Bounds the parts so the longest derived name stays under 63 characters.

// DerivedName builds the name of an internal object derived from a source of the application
// family. The hash covers the kind, the namespace and the name; a cluster source has no namespace part.
func DerivedName(prefix, kind, namespace, name string) string {
	hash := GetHash(kind + "/" + namespace + "/" + name)

	parts := []string{prefix}
	if namespace != "" {
		parts = append(parts, truncatePart(namespace))
	}
	parts = append(parts, truncatePart(name), hash)

	return strings.Join(parts, "-")
}

func truncatePart(part string) string {
	if len(part) > derivedPartLimit {
		part = part[:derivedPartLimit]
	}

	// A trailing dash would double up on the join, and a trailing dot would start a DNS label.
	return strings.TrimRight(part, "-.")
}

const helmReleaseNameLimit = 53 // The longest release name Helm accepts.

const releaseReadableLimit = helmReleaseNameLimit - len("-") - hashLength

// HelmReleaseName bounds a release name to what Helm accepts: a name within the limit is used
// as is, a longer one is cut and suffixed with a hash of the full name.
func HelmReleaseName(name string) string {
	if len(name) <= helmReleaseNameLimit {
		return name
	}

	return cutForHash(name) + "-" + GetHash(name)
}

// HashedReleaseName bounds a release name the same way but always carries the hash, which
// makes it injective where HelmReleaseName, having two branches, is not.
func HashedReleaseName(name string) string {
	return cutForHash(name) + "-" + GetHash(name)
}

func cutForHash(name string) string {
	if len(name) > releaseReadableLimit {
		name = name[:releaseReadableLimit]
	}

	// A trailing dash would double up against the suffix, and a trailing dot would start a DNS label.
	return strings.TrimRight(name, "-.")
}
