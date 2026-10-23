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

package internalobj

import (
	"time"

	helmv2 "github.com/fluxcd/helm-controller/api/v2"
	"github.com/fluxcd/pkg/apis/meta"
)

// ChartOutcome is the result of reconciling the internal HelmChart of a release.
// Err is a cluster write failure; Internal is what the object reported afterwards.
type ChartOutcome struct {
	Err      error
	Artifact *meta.Artifact
	Internal InternalObjectState
}

// OCIRepoOutcome is the result of reconciling the internal OCIRepository of a release.
// It also carries the verdict of the artifact probe, which runs before the object is written.
type OCIRepoOutcome struct {
	Err      error
	Artifact *meta.Artifact
	Internal InternalObjectState

	ProbeErr     error
	ProbeReason  string
	ProbeMessage string
	// ProbeTerminal marks a verdict about the artifact itself — the registry rejected the
	// request, or what it serves is not a packaged chart — and is reported as Stalled.
	ProbeTerminal bool
	// ProbeRequeueAfter asks for another pass when the probe failed for a reason that may
	// pass on its own. It is left unset by a verdict about the artifact itself.
	ProbeRequeueAfter time.Duration

	// VersionRemoved reports that the catalog still records the version while the
	// repository no longer offers the tag. Version and RepositoryName are set with it.
	VersionRemoved bool
	Version        string
	RepositoryName string
}

// ReleaseOutcome is the result of reconciling the internal HelmRelease. ChartDeployed
// reports whether the revision the release has deployed is the one the spec asks for.
type ReleaseOutcome struct {
	Err           error
	History       helmv2.Snapshots
	Internal      InternalObjectState
	ChartDeployed bool
}

// MaintenanceOutcome is the result of moving the internal HelmRelease into or out of
// maintenance mode. Activated is the state that was applied.
type MaintenanceOutcome struct {
	Err       error
	Activated bool
}

// InternalRepositoryState describes the observed state of the internal FluxCD repository
// object. Present is false for OCI repositories, which have none at the repository level.
type InternalRepositoryState struct {
	Present bool
	Ready   bool
	Stalled bool
	Reason  string
	Message string
}
