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

package reposync

// FetchOutcome is the result of reading the chart catalog from the remote
// repository. Terminal marks a failure that will not resolve by retrying. Pending
// counts the chart versions that were listed but could not be examined in this pass:
// they are retried by the next synchronization and are not failures.
type FetchOutcome struct {
	Err      error
	Terminal bool
	Reason   string
	Message  string
	Pending  int
	// Unexamined counts the chart versions the remote offers that the pass reached no
	// verdict about at all, because it ran out of its own budget before getting to
	// them. It is not a failure and not a pending verdict: nothing was recorded for
	// those versions, so a positive count means the catalog advanced but does not yet
	// mirror the repository, and another pass is due sooner than the usual interval.
	Unexamined int
	// Charts is how many charts the repository listed. It is meaningful only when the
	// read succeeded; a failed one leaves it zero, which is why the status field it
	// feeds is only written on success.
	Charts int
}

// CatalogOutcome is the result of writing the chart catalog into the cluster.
type CatalogOutcome struct {
	Err error
	// SkippedCharts names the charts that were listed by the remote but have no
	// catalog object, because the API cannot hold their names. They are not
	// failures, and they are also not part of the catalog: a caller reporting how
	// many charts the cluster shows has to subtract them.
	SkippedCharts []string
}

// SyncOutcome carries both phases of a synchronization attempt. FetchAttempted is
// true exactly when the registry was actually contacted (fetchCharts ran), on both
// its success and its failure path. It is false when a cluster-side read (such as
// knownCharts) failed before the fetch was ever issued: without this field the
// caller cannot tell that case apart from a zero-value, already-succeeded fetch,
// and would report a repository read failure as a read success.
type SyncOutcome struct {
	FetchAttempted bool
	// ForceRequestRecorded says a force request, if there was one, is now written into
	// the catalog and the annotation carrying it may be dropped. It is false only when
	// that write failed, which is the one case where the annotation is all that is left
	// of the request: consuming it then would drop the refresh on the floor, and the
	// repository would go on reporting successful synchronizations without ever
	// re-examining the versions the user forced the refresh for.
	ForceRequestRecorded bool
	Fetch                FetchOutcome
	Catalog              CatalogOutcome
}
