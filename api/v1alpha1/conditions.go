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

package v1alpha1

// The descriptions below say what a reader learns from seeing the condition or the
// reason on an object, in the words of the code that sets it. They are the contract
// the controllers are held to: a reason means the one situation described here, and
// a situation that is not described here needs a reason of its own.

const (
	// ConditionTypeManaged is False while the release is in maintenance mode, which
	// is the one state in which the controller leaves the deployed resources alone.
	ConditionTypeManaged = "Managed"
	// ConditionTypeInstalled reports the first deployment of a release. It is the
	// only condition a release that has never been installed carries besides Ready.
	ConditionTypeInstalled = "Installed"
	// ConditionTypeUpdateInstalled reports the deployment of a chart change on a
	// release that is already installed: a new chart name, version or repository.
	ConditionTypeUpdateInstalled = "UpdateInstalled"
	// ConditionTypeConfigurationApplied reports the deployment of a values change on
	// a release that is already installed.
	ConditionTypeConfigurationApplied = "ConfigurationApplied"
	// ConditionTypePartiallyDegraded is reserved for a release that works while part
	// of it does not. No controller sets it yet.
	ConditionTypePartiallyDegraded = "PartiallyDegraded"
	// ConditionTypeReady is the verdict of the last pass, carried by every kind. It
	// is Unknown while the pass has no verdict yet, False once anything failed.
	ConditionTypeReady = "Ready"
	// ConditionTypeSynced reports whether the chart catalog mirrors what the
	// repository offers. It is carried by the repository kinds only.
	ConditionTypeSynced = "Synced"
	// ConditionTypeUninstallFailed is raised while an owner is being deleted and the
	// Helm release under it cannot be uninstalled. Until then the owner stays
	// Reconciling and the condition is Unknown.
	ConditionTypeUninstallFailed = "UninstallFailed"

	// kstatus abnormal-true conditions. Present only while applicable.

	// ConditionTypeReconciling means work is in flight or is going to be retried, so
	// the current status is not the last word on the object.
	ConditionTypeReconciling = "Reconciling"
	// ConditionTypeStalled means the pass failed in a way that retrying cannot fix.
	// Progress needs a change to the object, to what it references, or to the remote
	// side. A release kind schedules no requeue for it and waits to be woken by such
	// a change. A repository kind keeps synchronizing on its backed-off schedule
	// instead, because the remote side it reports on can start answering without
	// anything here changing; only a spec it cannot act on at all, an unsupported
	// repository type, leaves it with nothing scheduled.
	ConditionTypeStalled = "Stalled"

	// ReasonMaintenanceModeActive marks a release whose reconciliation is suspended
	// by spec.maintenance, leaving the deployed resources open to manual edits.
	ReasonMaintenanceModeActive = "MaintenanceModeActive"
	// ReasonMaintenanceModeInactive marks a release reconciled the ordinary way,
	// which is the state spec.maintenance returns it to when it is cleared.
	ReasonMaintenanceModeInactive = "MaintenanceModeInactive"
	// ReasonSyncFailed marks a catalog left untouched because the repository could
	// not be read at all; what it holds is whatever the last successful read left.
	ReasonSyncFailed = "SyncFailed"
	// ReasonRepositoryNotReady is kept for a release blocked on a repository that is
	// not ready. Nothing sets it: a release reports that through the failure of the
	// step that needed the repository.
	ReasonRepositoryNotReady = "RepositoryNotReady"
	// ReasonReconciling marks work that is in flight: a step the controller started
	// and whose outcome is not known yet, including the wait for an internal object
	// to become ready and the wait for a deletion to finish.
	ReasonReconciling = "Reconciling"
	// ReasonSuccess marks a step that finished doing what the spec asks for.
	ReasonSuccess = "Success"
	// ReasonFailed marks a failure the code has no more specific reason for, such as
	// an API error while reconciling an internal object or a namespace. It is
	// retried unless the failure is reported as terminal.
	ReasonFailed = "Failed"
	// ReasonUninstallFailed marks an owner whose Helm release reported its own
	// uninstall as failed while the owner was being deleted.
	ReasonUninstallFailed = "UninstallFailed"
	// ReasonChartClaimConflict marks a release that cannot take the repository/chart
	// pair it asks for because another release of the same kind already holds it.
	// Only one addon may deploy a given chart of a given repository at a time.
	ReasonChartClaimConflict = "ChartClaimConflict"
	// ReasonRBACSetupFailed marks a release whose identity — the ServiceAccount,
	// Role and RoleBinding the chart is applied with — could not be reconciled.
	ReasonRBACSetupFailed = "RBACSetupFailed"
	// ReasonForeignRBACObject marks a release whose identity cannot be built
	// because a Role or RoleBinding already occupies the name it needs and does not
	// carry the operator's managed-by label. Such an object belongs to whoever
	// created it and is never adopted, patched or deleted.
	ReasonForeignRBACObject = "ForeignRBACObject"
	// ReasonForceReconcile marks the Reconciling condition raised for a pass that
	// was requested through the force reconcile annotation.
	ReasonForceReconcile = "ForceReconcile"
	// ReasonSynchronization marks the Reconciling condition raised for a pass that
	// runs on the ordinary synchronization schedule rather than on request.
	ReasonSynchronization = "Synchronization"

	// Repository condition reasons, shared by all three repository kinds.

	// ReasonAuxiliaryResourcesFailed marks a repository whose auxiliary objects —
	// the Secrets holding its credentials and its CA certificate — could not be
	// reconciled, so the repository cannot be read with the spec it now carries.
	ReasonAuxiliaryResourcesFailed = "AuxiliaryResourcesFailed"
	// ReasonCatalogUpdateFailed marks a repository that was read but whose chart
	// catalog could not be written. What the catalog holds is the older read.
	ReasonCatalogUpdateFailed = "CatalogUpdateFailed"
	// ReasonAwaitingInitialSync marks a repository that has never been read
	// successfully, so nothing is known about the charts it offers yet.
	ReasonAwaitingInitialSync = "AwaitingInitialSync"
	// ReasonProgressingWithRetry marks a failure the controller has scheduled
	// another attempt for: the repository read, the auxiliary resources or the
	// catalog update failed, and the backoff has not run out.
	ReasonProgressingWithRetry = "ProgressingWithRetry"
	// ReasonRetriesExceeded marks a repository whose reads have failed enough times
	// in a row (MaxFetchFailures) that the failure is reported as more than a passing
	// one. The retries go on at the backed-off synchronization cadence: a change to
	// the repository or its credentials is the reliable way back, but a remote side
	// that recovers on its own clears it too.
	ReasonRetriesExceeded = "RetriesExceeded"
	// ReasonAuthenticationFailed marks a repository that rejected the credentials it
	// was read with (HTTP 401 or 403).
	ReasonAuthenticationFailed = "AuthenticationFailed"
	// ReasonSourceNotFound marks a repository, or a chart reference taken from its
	// index, that the remote side does not have (HTTP 404).
	ReasonSourceNotFound = "SourceNotFound"
	// ReasonSourceRejectedRequest marks a repository that refused the read with a
	// client error other than the ones above; the message carries the status code.
	ReasonSourceRejectedRequest = "SourceRejectedRequest"
	// ReasonInvalidRepositoryURL marks a spec.url, or a chart reference derived from
	// it, that cannot be parsed as the kind of address its scheme promises.
	ReasonInvalidRepositoryURL = "InvalidRepositoryURL"
	// ReasonUnsupportedRepositoryType marks a spec.url whose scheme names no
	// repository type the operator can read.
	ReasonUnsupportedRepositoryType = "UnsupportedRepositoryType"
	// ReasonChartVersionRemoved marks an addon whose chart version is still recorded in
	// the catalog but is no longer offered by the repository.
	ReasonChartVersionRemoved = "ChartVersionRemoved"
	// ReasonPartialSync marks a repository whose catalog does not mirror everything
	// the repository offers. The two cases it covers need different reactions, so the
	// message says which one it is: chart versions an earlier read left unresolved may
	// resolve on a later pass, with no guarantee that they will, while a chart left out
	// because its name cannot be recorded on a catalog object never can — the name has
	// to change in the repository.
	ReasonPartialSync = "PartialSync"

	// HelmRelease error reasons. Each mirrors a condition of the internal HelmRelease
	// onto the release kind that owns it.

	// ReasonReleaseFailed marks a release that is not deployed: either the internal
	// HelmRelease reports a failed deployment, and the message is the one it reports,
	// or that object could not be created or updated at all, and the message is the
	// API error.
	ReasonReleaseFailed = "ReleaseFailed"
	// ReasonTestFailed marks a release that was deployed but whose Helm tests did
	// not pass.
	ReasonTestFailed = "TestFailed"
	// ReasonRemediated marks a release helm-controller rolled back or uninstalled
	// after a failed deployment, so what runs is not what the spec asks for.
	ReasonRemediated = "Remediated"

	// HelmChart error reasons. Each mirrors a condition of the internal HelmChart the
	// release is deployed from.

	// ReasonHelmChartFailed marks a chart the release cannot be deployed from: either
	// the internal HelmChart reports no error of its own yet has produced no
	// artifact, or that object could not be created or updated at all, and the
	// message is the API error.
	ReasonHelmChartFailed = "HelmChartFailed"
	// ReasonChartFetchFailed marks a chart source-controller could not pull from the
	// repository.
	ReasonChartFetchFailed = "ChartFetchFailed"
	// ReasonChartStorageFailed marks a chart that was pulled but could not be stored
	// by source-controller, so no artifact is available.
	ReasonChartStorageFailed = "ChartStorageFailed"

	// OCIRepository error reasons. Each mirrors a condition of the internal
	// OCIRepository an oci:// chart is pulled through.

	// ReasonOCIFetchFailed marks an artifact source-controller could not pull from
	// the registry.
	ReasonOCIFetchFailed = "OCIFetchFailed"
	// ReasonOCIIncludeUnavailable marks an artifact that cannot be assembled because
	// something it includes is unavailable.
	ReasonOCIIncludeUnavailable = "OCIIncludeUnavailable"
	// ReasonOCIStorageFailed marks an artifact that was pulled but could not be
	// stored by source-controller.
	ReasonOCIStorageFailed = "OCIStorageFailed"
	// ReasonOCIVerificationFailed marks an artifact whose signature source-controller
	// was asked to verify and could not.
	ReasonOCIVerificationFailed = "OCIVerificationFailed"
	// ReasonUnsupportedChartArtifact means the tag an index entry points at was read
	// but the artifact behind it is not a packaged Helm chart. It is a verdict about
	// the artifact, so it is reported without scheduling a retry.
	ReasonUnsupportedChartArtifact = "UnsupportedChartArtifact"
)
