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

package services

// InternalRepositoryState describes the observed state of the internal FluxCD
// repository object. Present is false for OCI repositories: they have no
// internal object at the repository level, each addon creates its own.
type InternalRepositoryState struct {
	Present bool
	Ready   bool
	Stalled bool
	Reason  string
	Message string
}

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
}

// CatalogOutcome is the result of writing the chart catalog into the cluster.
type CatalogOutcome struct {
	Err error
}

// ConfigOutcome is a terminal configuration failure detected before any attempt
// to reach the repository, such as an unsupported url scheme.
type ConfigOutcome struct {
	Reason  string
	Message string
	Err     error
}

// SyncOutcome carries both phases of a synchronization attempt. FetchAttempted is
// true exactly when the registry was actually contacted (fetchCharts ran), on both
// its success and its failure path. It is false when a cluster-side read (such as
// knownCharts) failed before the fetch was ever issued: without this field the
// caller cannot tell that case apart from a zero-value, already-succeeded fetch,
// and would report a repository read failure as a read success.
type SyncOutcome struct {
	FetchAttempted bool
	Fetch          FetchOutcome
	Catalog        CatalogOutcome
}
