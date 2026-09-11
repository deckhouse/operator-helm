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
// repository/chart pair across HelmClusterAddon objects. The name is a pure hash of
// the pair: repository and chart names may contain characters that are invalid in an
// object name (uppercase, dots for OCI references), and the hash is always a valid
// DNS subdomain.
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

// derivedPartLimit bounds the namespace and the name parts of a derived name. With
// the longest prefix in use ("hcapr-auth", 10 characters), two parts of this size, a
// 12-character hash and three dashes the result is 61 characters, under the
// 63-character limit shared by object names and label values.
const derivedPartLimit = 18

// DerivedName builds the name of an internal object derived from a source of the
// application family. Unlike the addon scheme above, the hash is always present and
// covers the kind, the namespace and the name of the source: internal objects of
// every family share one namespace, so two same-named sources in different
// namespaces, or in different kinds, must never derive the same internal name. The
// namespace part is omitted for a cluster-scoped source.
//
// The addon functions above keep their own scheme on purpose: their output names
// live objects, and changing it would re-create them.
func DerivedName(prefix, kind, namespace, name string) string {
	hash := GetHash(kind + "/" + namespace + "/" + name)

	parts := []string{prefix}
	if namespace != "" {
		parts = append(parts, truncatePart(namespace))
	}
	parts = append(parts, truncatePart(name), hash)

	return strings.Join(parts, "-")
}

// truncatePart cuts a name part to derivedPartLimit and drops a dash or a dot the
// cut may have left at the end. A dash would double up when the parts are joined;
// a dot would put the separator at the start of a DNS label, which the API server
// rejects. Object names carry dots because a resource name is a DNS subdomain.
func truncatePart(part string) string {
	if len(part) > derivedPartLimit {
		part = part[:derivedPartLimit]
	}

	return strings.TrimRight(part, "-.")
}

// helmReleaseNameLimit is the longest release name Helm accepts.
const helmReleaseNameLimit = 53

// HelmReleaseName bounds a release name to what Helm accepts. A name within the
// limit is used as is — that keeps every existing addon release untouched — and a
// longer one is cut to 40 characters and suffixed with a 12-character hash of the
// full name, so two long names that share a prefix stay distinct. The cut is
// trimmed of a trailing dash or dot: a dash would double up against the suffix,
// and a dot would leave the suffix starting a DNS label, which is not a valid name.
func HelmReleaseName(name string) string {
	if len(name) <= helmReleaseNameLimit {
		return name
	}

	return strings.TrimRight(name[:40], "-.") + "-" + GetHash(name)
}
