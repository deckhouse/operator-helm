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

const applicationDerivedPartLimit = 18

func ApplicationServiceAccountName(namespace, name string) string {
	sum := sha256.Sum256([]byte("HelmApplication/" + namespace + "/" + name))

	return strings.Join([]string{
		"hap",
		truncateNamePart(namespace),
		truncateNamePart(name),
		fmt.Sprintf("%x", sum[:])[:12],
	}, "-")
}

func truncateNamePart(part string) string {
	if len(part) > applicationDerivedPartLimit {
		part = part[:applicationDerivedPartLimit]
	}

	return strings.TrimRight(part, "-.")
}

const helmReleaseNameLimit = 53

func ApplicationReleaseName(name string) string {
	full := "hap-" + name

	readable := full
	if len(readable) > helmReleaseNameLimit-13 {
		readable = readable[:helmReleaseNameLimit-13]
	}

	sum := sha256.Sum256([]byte(full))

	return strings.TrimRight(readable, "-.") + "-" + fmt.Sprintf("%x", sum[:])[:12]
}

func ApplicationRepositoryInternalName(namespace, name string) string {
	sum := sha256.Sum256([]byte("HelmApplicationRepository/" + namespace + "/" + name))

	return strings.Join([]string{
		"hapr",
		truncateNamePart(namespace),
		truncateNamePart(name),
		fmt.Sprintf("%x", sum[:])[:12],
	}, "-")
}
