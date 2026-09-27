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
//
// unexamined names the versions the fetch offers no verdict about because it ran out of
// its budget before reaching them. They are not absent from the repository and must not
// be read as such: whatever a previous pass recorded for them is kept verbatim, which is
// both the incremental state the next pass carries forward and the only honest answer
// about a version nobody looked at. A version that is unexamined and unrecorded stays
// out of the status entirely rather than being invented as pending.
//
// forced says this pass read the repository in full, ignoring every recorded verdict,
// which is what someone asks for by forcing a refresh. It is the one thing that marks a
// recorded version for re-examination, and it marks exactly the ones it did not reach:
// an ordinary pass skips a version whose media type is recorded, so without the mark the
// unreached half of a forced refresh is silently dropped and a tag that was re-pushed —
// the reason to force in the first place — is never looked at again. The mark is a note
// about the verdict, not a verdict: the entry keeps everything it recorded, so a release
// already deploying that version still resolves it while the refresh finishes.
//
// Every version this pass did put a verdict on has the mark cleared, whether the pass
// was forced or not: a pass that examined a version has answered the question the mark
// asked. So is a version that is gone from the repository — there is nothing left to
// examine, and the RemovedFromRepository entry a consumer keeps alive must not carry a
// request for a pass that can never clear it.
func mergeChartVersions(
	fetched []repoclient.ChartVersion,
	current []helmv1alpha1.ChartVersion,
	inUse map[string]struct{},
	unexamined []string,
	forced bool,
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
		if _, notExamined := deferred[version.Version]; notExamined {
			// The repository still offers this version; this pass simply never got to
			// it. Marking it removed would be a lie told by our own budget, and dropping
			// it would throw away the verdict the next pass is meant to skip. A forced
			// pass leaves behind the one thing that does change: the verdict it was asked
			// to replace and did not get to is now known to be stale.
			version.ReexaminationRequired = version.ReexaminationRequired || forced
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
