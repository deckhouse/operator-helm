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
	// Helm release under it cannot be uninstalled.
	ConditionTypeUninstallFailed = "UninstallFailed"

	// kstatus abnormal-true conditions. Present only while applicable.

	// ConditionTypeReconciling means work is in flight or is going to be retried, so
	// the current status is not the last word on the object.
	ConditionTypeReconciling = "Reconciling"
	// ConditionTypeStalled means the pass failed in a way that retrying cannot fix.
	// Progress needs a change to the object, to what it references, or to the remote side.
	ConditionTypeStalled = "Stalled"

	// ReasonMaintenanceModeActive marks a release whose reconciliation is suspended
	// by spec.maintenance, leaving the deployed resources open to manual edits.
	ReasonMaintenanceModeActive = "MaintenanceModeActive"
	// ReasonMaintenanceModeInactive marks a release reconciled the ordinary way,
	// which is the state spec.maintenance returns it to when it is cleared.
	ReasonMaintenanceModeInactive = "MaintenanceModeInactive"
	// ReasonSyncFailed marks a catalog left untouched because the repository could
	// not be read at all.
	ReasonSyncFailed = "SyncFailed"
	// ReasonRepositoryNotReady is kept for a release blocked on a repository that is
	// not ready. Nothing sets it.
	ReasonRepositoryNotReady = "RepositoryNotReady"
	// ReasonReconciling marks a step the controller started and whose outcome is not
	// known yet, including a wait for an internal object or for a deletion.
	ReasonReconciling = "Reconciling"
	// ReasonSuccess marks a step that finished doing what the spec asks for.
	ReasonSuccess = "Success"
	// ReasonFailed marks a failure the code has no more specific reason for, such as
	// an API error while reconciling an internal object or a namespace.
	ReasonFailed = "Failed"
	// ReasonUninstallFailed marks an owner whose Helm release reported its own
	// uninstall as failed while the owner was being deleted.
	ReasonUninstallFailed = "UninstallFailed"
	// ReasonChartClaimConflict marks a release that cannot take the repository/chart
	// pair it asks for because another release of the same kind already holds it.
	ReasonChartClaimConflict = "ChartClaimConflict"
	// ReasonRBACSetupFailed marks a release whose identity — the ServiceAccount,
	// Role and RoleBinding the chart is applied with — could not be reconciled.
	ReasonRBACSetupFailed = "RBACSetupFailed"
	// ReasonForeignRBACObject marks a release whose identity cannot be built because
	// a Role or RoleBinding occupies the name it needs without the managed-by label.
	ReasonForeignRBACObject = "ForeignRBACObject"
	// ReasonForceReconcile marks the Reconciling condition raised for a pass that
	// was requested through the force reconcile annotation.
	ReasonForceReconcile = "ForceReconcile"
	// ReasonSynchronization marks the Reconciling condition raised for a pass that
	// runs on the ordinary synchronization schedule rather than on request.
	ReasonSynchronization = "Synchronization"

	// Repository condition reasons, shared by all three repository kinds.

	// ReasonAuxiliaryResourcesFailed marks a repository whose auxiliary objects — the
	// Secrets holding its credentials and its CA certificate — failed to reconcile.
	ReasonAuxiliaryResourcesFailed = "AuxiliaryResourcesFailed"
	// ReasonCatalogUpdateFailed marks a repository that was read but whose chart
	// catalog could not be written.
	ReasonCatalogUpdateFailed = "CatalogUpdateFailed"
	// ReasonAwaitingInitialSync marks a repository that has never been read
	// successfully, so nothing is known about the charts it offers yet.
	ReasonAwaitingInitialSync = "AwaitingInitialSync"
	// ReasonProgressingWithRetry marks a failed read, auxiliary resource or catalog
	// update the controller has scheduled another attempt for.
	ReasonProgressingWithRetry = "ProgressingWithRetry"
	// ReasonRetriesExceeded marks a repository whose reads have failed enough times
	// in a row (MaxFetchFailures) that the failure is more than a passing one.
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
	// ReasonInvalidCACertificate marks a spec.caCertificate that carries no PEM
	// encoded certificate.
	ReasonInvalidCACertificate = "InvalidCACertificate"
	// ReasonChartVersionRemoved marks an addon whose chart version is still recorded in
	// the catalog but is no longer offered by the repository.
	ReasonChartVersionRemoved = "ChartVersionRemoved"
	// ReasonChartVersionNotFound marks a release the repository has answered in full
	// and cannot serve; the message says why.
	ReasonChartVersionNotFound = "ChartVersionNotFound"
	// ReasonPartialSync marks a repository whose catalog does not mirror everything
	// the repository offers; the message says what was left out.
	ReasonPartialSync = "PartialSync"

	// HelmRelease error reasons.

	// ReasonReleaseFailed marks a release that is not deployed: the internal HelmRelease
	// reports a failed deployment, or it could not be created or updated at all.
	ReasonReleaseFailed = "ReleaseFailed"
	// ReasonTestFailed marks a release that was deployed but whose Helm tests did
	// not pass.
	ReasonTestFailed = "TestFailed"
	// ReasonRemediated marks a release helm-controller rolled back or uninstalled
	// after a failed deployment, so what runs is not what the spec asks for.
	ReasonRemediated = "Remediated"

	// HelmChart error reasons.

	// ReasonHelmChartFailed marks a chart the release cannot be deployed from because
	// the internal HelmChart could not be created or updated at all.
	ReasonHelmChartFailed = "HelmChartFailed"
	// ReasonChartFetchFailed marks a chart source-controller could not pull from the
	// repository.
	ReasonChartFetchFailed = "ChartFetchFailed"
	// ReasonChartStorageFailed marks a chart that was pulled but could not be stored
	// by source-controller, so no artifact is available.
	ReasonChartStorageFailed = "ChartStorageFailed"

	// OCIRepository error reasons.

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
	// but the artifact behind it is not a packaged Helm chart.
	ReasonUnsupportedChartArtifact = "UnsupportedChartArtifact"
)
