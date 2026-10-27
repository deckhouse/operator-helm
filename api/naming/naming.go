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

// Prefixes of the internal object names derived from the sources of the application family.
const (
	ApplicationPrefix                      = "hap"
	ApplicationRepositoryPrefix            = "hapr"
	ApplicationRepositoryAuthPrefix        = "hapr-auth"
	ApplicationRepositoryTLSPrefix         = "hapr-tls"
	ClusterApplicationRepositoryPrefix     = "hcapr"
	ClusterApplicationRepositoryAuthPrefix = "hcapr-auth"
	ClusterApplicationRepositoryTLSPrefix  = "hcapr-tls"
)

// HelmClusterAddonChartName derives the name of the HelmClusterAddonChart object
// that mirrors one chart of a repository.
func HelmClusterAddonChartName(repoName, chartName string) string {
	return chartObjectName(repoName, chartName)
}

// HelmApplicationChartName derives the name of the HelmApplicationChart object that
// mirrors one chart of a HelmApplicationRepository.
func HelmApplicationChartName(repoName, chartName string) string {
	return chartObjectName(repoName, chartName)
}

// HelmClusterApplicationChartName derives the name of the HelmClusterApplicationChart
// object that mirrors one chart of a HelmClusterApplicationRepository.
func HelmClusterApplicationChartName(repoName, chartName string) string {
	return chartObjectName(repoName, chartName)
}

const derivedPartLimit = 18 // Truncates the namespace and the name; with the prefixes in use the result stays under 63 characters.

// DerivedName builds the name of an internal object derived from a source of the application
// family. The hash covers the kind, the namespace and the name; a cluster source has no namespace part.
func DerivedName(prefix, kind, namespace, name string) string {
	hash := hash(kind + "/" + namespace + "/" + name)

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

	return cutForHash(name) + "-" + hash(name)
}

// HashedReleaseName bounds a release name the same way but always carries the hash, which
// makes it injective where HelmReleaseName, having two branches, is not.
func HashedReleaseName(name string) string {
	return cutForHash(name) + "-" + hash(name)
}

func cutForHash(name string) string {
	if len(name) > releaseReadableLimit {
		name = name[:releaseReadableLimit]
	}

	// A trailing dash would double up against the suffix, and a trailing dot would start a DNS label.
	return strings.TrimRight(name, "-.")
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

const hashLength = 12

func hash(s string) string {
	h := sha256.New()
	h.Write([]byte(s))

	return fmt.Sprintf("%x", h.Sum(nil))[:hashLength]
}
