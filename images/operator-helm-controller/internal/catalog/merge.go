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

// mergeChartVersions builds the desired version list from the fetched entries and the
// ones already recorded. A recorded version the registry no longer lists is dropped,
// unless a consumer still references it: then it is retained with RemovedFromRepository
// and keeps both its media type and its recorded OCI reference, without either of
// which the consumer's internal OCIRepository could not be built at all.
//
// The same protection applies to a version that is still listed but whose tag was
// re-pushed as a non-chart artifact: the fresh verdict carries no media type, but if a
// consumer still references the version, its previously recorded media type is carried
// forward alongside the fresh UnsupportedMediaType reason and message. Without the old
// media type the internal OCIRepository could not be built at all, which would block
// every change to the running consumer (values, maintenance mode, ...) rather than just
// the pull that the new artifact actually breaks; the real pull failure is reported by
// the source controller instead.
func mergeChartVersions(
	fetched []repoclient.ChartVersion,
	current []helmv1alpha1.ChartVersion,
	inUse map[string]struct{},
) []helmv1alpha1.ChartVersion {
	merged := make([]helmv1alpha1.ChartVersion, 0, len(fetched)+len(current))
	listed := make(map[string]struct{}, len(fetched))

	currentByVersion := make(map[string]helmv1alpha1.ChartVersion, len(current))
	for _, version := range current {
		currentByVersion[version.Version] = version
	}

	for _, version := range fetched {
		name := version.Version.Original()
		listed[name] = struct{}{}

		mediaType := version.MediaType
		// The carry-forward only makes sense for a version that still resolves to an
		// archive: a fresh entry that now carries an OCIRef must probe its own layer
		// media type from scratch, or a stale value stamped here would be read by
		// resolveMediaType before the force-reconcile cache bypass and the pull would
		// fail forever with no way to correct it.
		if mediaType == "" && version.OCIRef == "" {
			if _, referenced := inUse[name]; referenced {
				if old, recorded := currentByVersion[name]; recorded && old.MediaType != "" {
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
		if _, referenced := inUse[version.Version]; !referenced {
			continue
		}

		version.UnavailableReason = helmv1alpha1.UnavailableReasonRemovedFromRepository
		version.UnavailableMessage = "the repository no longer offers this version"
		merged = append(merged, version)
	}

	sortChartVersions(merged)

	return merged
}

// sortChartVersions orders versions by descending semver, breaking ties by a reverse
// string comparison. A version that does not parse as semver sorts after every
// version that does, ordered among themselves by the same reverse string comparison.
// Parsability has to be the primary key: comparing a parsable and an unparsable
// version by semver on one pair and by string on another can produce a cycle (e.g.
// "6.10.0" > "6.9.0" by semver, "6.9.0" > "6.5.x" and "6.5.x" > "6.10.0" by string),
// which is not a valid ordering for sort.SliceStable. Today's clients never write an
// unparsable version, but legacy status data can still carry one, and the order has to
// be deterministic regardless: the merge goes through maps, and an unstable order
// would produce a status patch on every synchronization for a catalog that did not
// change.
func sortChartVersions(versions []helmv1alpha1.ChartVersion) {
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
