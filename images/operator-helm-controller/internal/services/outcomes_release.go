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

// OCIRepoOutcome is the result of reconciling the internal OCIRepository of a
// release. Besides the write failure and the object's own state it carries the
// verdict of the artifact probe, which runs before the object is written and can
// fail on its own.
type OCIRepoOutcome struct {
	Err      error
	Artifact *meta.Artifact
	Internal InternalObjectState

	ProbeErr     error
	ProbeReason  string
	ProbeMessage string
	// ProbeTerminal marks a verdict about the artifact itself — the registry rejected
	// the request, or what it serves is not a packaged chart. Nothing about the
	// release will make such a pull succeed, so it is reported as Stalled.
	ProbeTerminal bool
	// ProbeRequeueAfter asks for another pass when the probe failed for a reason that
	// may pass on its own. It is left unset by a verdict about the artifact itself,
	// which no amount of retrying will change; there is no watch on a foreign
	// registry, so this timer is the only thing that brings the other kind back.
	ProbeRequeueAfter time.Duration

	// VersionRemoved reports that the catalog still records the version — which is
	// what keeps the release reconcilable — while the repository no longer offers the
	// tag. The pull cannot succeed, and naming that cause is better than leaving only
	// the source controller's "not found". Version and RepositoryName are set with it,
	// to name the pair in the message.
	VersionRemoved bool
	Version        string
	RepositoryName string
}

// ReleaseOutcome is the result of reconciling the internal HelmRelease.
// ChartDeployed reports whether the revision the release currently has deployed is
// the one the spec asks for: a chart-version change moves the artifact without
// touching the HelmRelease spec or generation, so the object can still report the
// previous revision as ready.
type ReleaseOutcome struct {
	Err           error
	History       helmv2.Snapshots
	Internal      InternalObjectState
	ChartDeployed bool
}

// AccessOutcome is the result of reconciling the identity a release is applied
// with. Terminal marks a failure that will not resolve by retrying: an object the
// identity needs already occupies its name and is not the operator's to touch.
// Reason and Message are what the release reports, filled in by the service so both
// classes of failure are named where they are recognized.
type AccessOutcome struct {
	Err      error
	Terminal bool
	Reason   string
	Message  string
}

// MaintenanceOutcome is the result of moving the internal HelmRelease into or out of
// maintenance mode. Activated is the state that was applied.
type MaintenanceOutcome struct {
	Err       error
	Activated bool
}
