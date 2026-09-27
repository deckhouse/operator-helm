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
// An empty list of fetched entries is an input like any other, not a degenerate one:
// it is how a chart the repository stopped offering altogether is merged (recordAbsence),
// every recorded version of it being unlisted by definition.
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
// marked names the versions MarkForReexamination left owing a fresh verdict earlier in
// this same pass, and it is the only thing a deferred version's mark is taken from. The
// recorded entries this merge is handed come from a cached read that may predate that
// write, and a merge patch on a list ships the whole array, so trusting what the read
// carried would write the deferred versions back unmarked and lose a refresh whose
// annotation is already consumed. It is empty for an unforced pass, which marks nothing.
//
// The mark a forced refresh leaves on a recorded version (ReexaminationRequired) is
// only ever cleared here, never invented: MarkForReexamination puts it on every recorded
// version before the registry is read, so by the time a pass is merged the request is
// already written down for everything it could still owe. This merge decides which
// half of it is now done. A version this pass put a verdict on has the mark cleared —
// the question it asked has been answered — and a version this pass deferred keeps it,
// because that is the work the passes after this one have to finish. A version that is
// gone from the repository has it cleared too: a pass that saw the whole repository and
// did not list the version has answered the question the mark asked — there is no
// verdict left to refresh — and the RemovedFromRepository entry a consumer keeps alive
// must not go on claiming work in a status a user reads. Should the tag be pushed again,
// the pass that lists it examines it on the strength of that reason alone
// (FetchOptions.NeedsExamination), mark or no mark.
//
// Being among the fetched entries is not the same as having been examined, which is why
// only a version this pass actually probed has its mark cleared. A pass that did not have
// to examine a version carries the recorded verdict forward without asking the registry
// anything and reports it here like any other (repoclient.ChartVersion.Carried); the
// entry exists so the catalog keeps publishing the verdict, and a carry answers no
// question. Clearing a mark on one would end a refresh nobody did, and that is reachable
// rather than theoretical: an ordinary pass decides what to skip against the Known
// snapshot it was built from, which comes from the same lagging cache as the read below,
// so a snapshot taken before a marking write leaves the version skippable for that pass
// while the read this merge is computed from may already carry the mark. A carried entry
// therefore keeps whatever that read recorded instead of clearing it.
//
// That converges rather than pinning the mark. If the read carries no mark, preserving
// "no mark" writes the entry exactly as it already was — a no-op. If it carries one, the
// mark survives this pass, and the next pass builds its Known from a read that has caught
// up (the write it lagged behind is a whole synchronization interval old by then), so
// NeedsExamination sees the mark, the version is probed, and the mark is cleared by an
// answer. This merge never marks a version the marking pass did not: the only mark it
// writes is the one put back on a deferred version from the set that pass reported, so a
// version cannot be kept marked by this rule alone; a mark comes off by a probe, a
// confirmed absence or a drop.
func mergeChartVersions(
	fetched []repoclient.ChartVersion,
	current []helmv1alpha1.ChartVersion,
	inUse map[string]struct{},
	unexamined []string,
	marked map[string]struct{},
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

		// A carry is not an answer: the mark stays exactly as the read found it. A forced
		// pass never gets here — it asks for every tag (FetchOptions.Full), so nothing it
		// fetches is carried — which is why the set the marking reported is not consulted
		// for a fetched version the way it is for a deferred one. A version nothing is
		// recorded for needs no test of its own: old is the zero value then, and an entry
		// the catalog has never held carries no mark.
		reexaminationRequired := version.Carried && old.ReexaminationRequired

		mediaType := version.MediaType
		// The carry-forward only makes sense for a version that still resolves to an
		// archive: a fresh entry that now carries an OCIRef must probe its own layer
		// media type from scratch, or a stale value stamped here would be read by
		// resolveMediaType before the force-reconcile cache bypass and the pull would
		// fail forever with no way to correct it.
		if mediaType == "" && version.OCIRef == "" {
			if _, referenced := inUse[name]; referenced {
				if recorded && old.MediaType != "" {
					mediaType = old.MediaType
				}
			}
		}

		merged = append(merged, helmv1alpha1.ChartVersion{
			Version:               name,
			OCIRef:                version.OCIRef,
			MediaType:             mediaType,
			UnavailableReason:     version.UnavailableReason,
			UnavailableMessage:    version.UnavailableMessage,
			ReexaminationRequired: reexaminationRequired,
		})
	}

	for _, version := range current {
		if _, stillListed := listed[version.Version]; stillListed {
			continue
		}
		if _, notExamined := deferred[version.Version]; notExamined {
			// The repository still offers this version; this pass simply never got to
			// it. Marking it removed would be a lie told by our own budget, and dropping
			// it would throw away the verdict the next pass is meant to skip. The entry is
			// kept verbatim, mark included: a version an outstanding refresh still owes a
			// fresh verdict for is exactly one this pass did not get to, and clearing the
			// mark here would end the refresh without doing the work it asked for.
			if _, owed := marked[version.Version]; owed {
				version.ReexaminationRequired = true
			}

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
