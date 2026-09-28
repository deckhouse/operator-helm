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

package catalog

import (
	"sort"

	"github.com/Masterminds/semver/v3"

	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
	repoclient "github.com/deckhouse/operator-helm/internal/client/repository"
)

func mergeChartVersions(
	fetched []repoclient.ChartVersion,
	current []helmv1alpha1.ChartVersion,
	inUse map[string]struct{},
	unexamined []string,
) []helmv1alpha1.ChartVersion {
	merged := make([]helmv1alpha1.ChartVersion, 0, len(fetched)+len(current))
	listed := make(map[string]struct{}, len(fetched))

	deferred := make(map[string]struct{}, len(unexamined))
	for _, version := range unexamined {
		deferred[version] = struct{}{}
	}

	currentByVersion := make(map[string]helmv1alpha1.ChartVersion, len(current))
	for _, version := range current {
		currentByVersion[version.Version] = version
	}

	for _, version := range fetched {
		name := version.Version.Original()
		listed[name] = struct{}{}

		old, recorded := currentByVersion[name]

		mediaType := version.MediaType
		// A fresh entry that now carries an OCIRef must probe its own media type rather than inherit one.
		if mediaType == "" && version.OCIRef == "" {
			if _, referenced := inUse[name]; referenced {
				if recorded && old.MediaType != "" {
					mediaType = old.MediaType
				}
			}
		}

		merged = append(merged, helmv1alpha1.ChartVersion{
			Version:            name,
			OCIRef:             version.OCIRef,
			MediaType:          mediaType,
			UnavailableReason:  version.UnavailableReason,
			UnavailableMessage: version.UnavailableMessage,
		})
	}

	for _, version := range current {
		if _, stillListed := listed[version.Version]; stillListed {
			continue
		}
		if _, notExamined := deferred[version.Version]; notExamined {
			// The repository still offers this version; this pass never got to it, so the entry is kept verbatim.
			merged = append(merged, version)

			continue
		}
		if _, referenced := inUse[version.Version]; !referenced {
			continue
		}

		version.UnavailableReason = helmv1alpha1.UnavailableReasonRemovedFromRepository
		version.UnavailableMessage = "the repository no longer offers this version"
		version.ReexaminationRequired = false
		merged = append(merged, version)
	}

	sortChartVersions(merged)

	return merged
}

func sortChartVersions(versions []helmv1alpha1.ChartVersion) {
	// Parsability is the primary key: mixing semver and string comparison can produce a cycle.
	sort.SliceStable(versions, func(i, j int) bool {
		left, leftErr := semver.NewVersion(versions[i].Version)
		right, rightErr := semver.NewVersion(versions[j].Version)

		leftParses, rightParses := leftErr == nil, rightErr == nil

		if leftParses != rightParses {
			return leftParses
		}

		if leftParses && !left.Equal(right) {
			return left.GreaterThan(right)
		}

		return versions[i].Version > versions[j].Version
	})
}
