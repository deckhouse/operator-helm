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

// FetchOutcome is the result of reading the chart catalog from the remote repository.
// Terminal marks a failure that will not resolve by retrying.
type FetchOutcome struct {
	Err      error
	Terminal bool
	Reason   string
	Message  string
	// Pending counts the chart versions that were listed but could not be examined in this
	// pass. They are retried by the next synchronization and are not failures.
	Pending int
	// Unexamined counts the chart versions the remote offers that the pass reached no verdict
	// about at all, because it ran out of its own budget before getting to them.
	Unexamined int
	// Charts is how many charts the repository listed. It is meaningful only when the read
	// succeeded; a failed one leaves it zero.
	Charts int
}

// CatalogOutcome is the result of writing the chart catalog into the cluster.
type CatalogOutcome struct {
	Err error
	// SkippedCharts names the charts the remote listed that have no catalog object, because
	// the API cannot hold their names. A caller counting the catalog has to subtract them.
	SkippedCharts []string
}

// SyncOutcome carries both phases of a synchronization attempt. FetchAttempted is true
// exactly when the registry was contacted, on both its success and its failure path.
type SyncOutcome struct {
	FetchAttempted bool
	// ForceRequestRecorded says a force request, if there was one, is now written into the
	// catalog and the annotation carrying it may be dropped.
	ForceRequestRecorded bool
	Fetch                FetchOutcome
	Catalog              CatalogOutcome
}
