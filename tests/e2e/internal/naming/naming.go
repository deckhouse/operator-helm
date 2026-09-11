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

// Package naming reproduces the names operator-helm-controller derives for the
// internal objects of a source in the application family. It imports nothing from
// internal/framework, so it stays testable with no cluster and no config file
// present; internal/util calls into it for anything cluster-facing.
package naming

import (
	"crypto/sha256"
	"fmt"
	"strings"
)

// applicationDerivedPartLimit bounds the namespace and name parts of a derived
// name. It must match derivedPartLimit in
// images/operator-helm-controller/internal/utils/name.go.
const applicationDerivedPartLimit = 18

// ApplicationServiceAccountName reproduces the name operator-helm-controller
// derives for an application's internal objects — the ServiceAccount it is applied
// as, its RoleBinding, its HelmRelease. The scheme is
// "hap-<namespace|18>-<name|18>-<hash12>" over (kind, namespace, name).
//
// This is the twin of DerivedName("hap", "HelmApplication", ...) in TestDerivedName
// (images/operator-helm-controller/internal/utils/name_test.go): a change on either
// side that is not mirrored on the other breaks that test or
// TestApplicationServiceAccountName in this package.
func ApplicationServiceAccountName(namespace, name string) string {
	sum := sha256.Sum256([]byte("HelmApplication/" + namespace + "/" + name))

	return strings.Join([]string{
		"hap",
		truncateNamePart(namespace),
		truncateNamePart(name),
		fmt.Sprintf("%x", sum[:])[:12],
	}, "-")
}

// truncateNamePart cuts a name part to applicationDerivedPartLimit and drops a dash
// or a dot the cut may have left at the end, mirroring utils.truncatePart.
func truncateNamePart(part string) string {
	if len(part) > applicationDerivedPartLimit {
		part = part[:applicationDerivedPartLimit]
	}

	return strings.TrimRight(part, "-.")
}

// helmReleaseNameLimit is the longest release name Helm accepts. It must match
// helmReleaseNameLimit in
// images/operator-helm-controller/internal/utils/name.go.
const helmReleaseNameLimit = 53

// ApplicationReleaseName reproduces the Helm release name operator-helm-controller
// installs a HelmApplication's chart under: the twin of
// utils.HelmReleaseName("hap-"+name) in
// images/operator-helm-controller/internal/adapter/application_release.go
// (ApplicationRelease.ReleaseName). A name within the limit is used as is; a
// longer one is cut to 40 characters and suffixed with a 12-character hash of the
// full name, mirroring HelmReleaseName's own truncation branch.
//
// This is the twin of the "hap-prefixed name over the limit is cut and hashed"
// case in TestHelmReleaseName
// (images/operator-helm-controller/internal/utils/name_test.go): a change on
// either side that is not mirrored on the other breaks one of the two tests.
func ApplicationReleaseName(name string) string {
	full := "hap-" + name
	if len(full) <= helmReleaseNameLimit {
		return full
	}

	sum := sha256.Sum256([]byte(full))

	return strings.TrimRight(full[:40], "-.") + "-" + fmt.Sprintf("%x", sum[:])[:12]
}

// ApplicationRepositoryInternalName reproduces the name operator-helm-controller
// derives for a HelmApplicationRepository's internal HelmRepository. The scheme is
// "hapr-<namespace|18>-<name|18>-<hash12>" over (kind, namespace, name).
//
// This is the twin of DerivedName("hapr", "HelmApplicationRepository", ...) in
// TestDerivedName (images/operator-helm-controller/internal/utils/name_test.go): a
// change on either side that is not mirrored on the other breaks that test or
// TestApplicationRepositoryInternalName in this package.
func ApplicationRepositoryInternalName(namespace, name string) string {
	sum := sha256.Sum256([]byte("HelmApplicationRepository/" + namespace + "/" + name))

	return strings.Join([]string{
		"hapr",
		truncateNamePart(namespace),
		truncateNamePart(name),
		fmt.Sprintf("%x", sum[:])[:12],
	}, "-")
}
