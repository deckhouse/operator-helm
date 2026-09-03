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
	ConditionTypeManaged              = "Managed"
	ConditionTypeInstalled            = "Installed"
	ConditionTypeUpdateInstalled      = "UpdateInstalled"
	ConditionTypeConfigurationApplied = "ConfigurationApplied"
	ConditionTypePartiallyDegraded    = "PartiallyDegraded"
	ConditionTypeReady                = "Ready"
	ConditionTypeSynced               = "Synced"
	ConditionTypeUninstallFailed      = "UninstallFailed"

	// kstatus abnormal-true conditions. Present only while applicable.
	ConditionTypeReconciling = "Reconciling"
	ConditionTypeStalled     = "Stalled"

	ReasonMaintenanceModeActive   = "MaintenanceModeActive"
	ReasonMaintenanceModeInactive = "MaintenanceModeInactive"
	ReasonSyncFailed              = "SyncFailed"
	ReasonRepositoryNotReady      = "RepositoryNotReady"
	ReasonReconciling             = "Reconciling"
	ReasonSuccess                 = "Success"
	ReasonFailed                  = "Failed"
	ReasonUninstallFailed         = "UninstallFailed"
	ReasonChartClaimConflict      = "ChartClaimConflict"

	// HelmClusterAddonRepository condition reasons.
	ReasonAuxiliaryResourcesFailed  = "AuxiliaryResourcesFailed"
	ReasonCatalogUpdateFailed       = "CatalogUpdateFailed"
	ReasonAwaitingInitialSync       = "AwaitingInitialSync"
	ReasonProgressingWithRetry      = "ProgressingWithRetry"
	ReasonRetriesExceeded           = "RetriesExceeded"
	ReasonAuthenticationFailed      = "AuthenticationFailed"
	ReasonSourceNotFound            = "SourceNotFound"
	ReasonSourceRejectedRequest     = "SourceRejectedRequest"
	ReasonInvalidRepositoryURL      = "InvalidRepositoryURL"
	ReasonUnsupportedRepositoryType = "UnsupportedRepositoryType"
	// ReasonChartVersionRemoved marks an addon whose chart version is still recorded in
	// the catalog but is no longer offered by the repository.
	ReasonChartVersionRemoved = "ChartVersionRemoved"
	// ReasonPartialSync marks a repository whose first catalog read left some chart
	// versions unresolved.
	ReasonPartialSync = "PartialSync"

	// HelmRelease error reasons
	ReasonReleaseFailed = "ReleaseFailed"
	ReasonTestFailed    = "TestFailed"
	ReasonRemediated    = "Remediated"

	// HelmChart error reasons
	ReasonHelmChartFailed    = "HelmChartFailed"
	ReasonChartFetchFailed   = "ChartFetchFailed"
	ReasonChartStorageFailed = "ChartStorageFailed"

	// OCIRepository error reasons
	ReasonOCIFetchFailed        = "OCIFetchFailed"
	ReasonOCIIncludeUnavailable = "OCIIncludeUnavailable"
	ReasonOCIStorageFailed      = "OCIStorageFailed"
	ReasonOCIVerificationFailed = "OCIVerificationFailed"
)
