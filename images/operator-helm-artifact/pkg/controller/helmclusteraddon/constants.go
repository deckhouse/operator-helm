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

package helmclusteraddon

const (
	// ControllerName is the name of this controller, used for leader election and logging.
	ControllerName = "helmclusteraddon-controller"

	// TargetNamespace is the namespace where internal customer resources are created.
	TargetNamespace = "d8-operator-helm"

	// FinalizerName is the finalizer added to HelmClusterRepository to ensure cleanup.
	FinalizerName = "helm.deckhouse.io/cleanup"

	ConditionTypeReady                = "Ready"
	ConditionTypeManaged              = "Managed"
	ConditionTypeInstalled            = "Installed"
	ConditionTypeUpdateInstalled      = "UpdateInstalled"
	ConditionTypeConfigurationApplied = "ConfigurationApplied"
	ConditionTypePartiallyDegraded    = "PartiallyDegraded"

	ReasonInitializing           = "Initializing"
	ReasonUnmanagedModeActivated = "UnmanagedModeActivated"
	ReasonManagedModeActivated   = "ManagedModeActivated"
	ReasonInstallationInProgress = "InstallationInProgress"
	ReasonDownloading            = "Downloading"
	ReasonDownloadWasFailed      = "DownloadWasFailed"
	ReasonUpdateInProgress       = "UpdateInProgress"
	ReasonUpdateFailed           = "UpdateFailed"

	// ReasonMirrorSucceeded indicates the internal HelmRepository was created/updated successfully.
	ReasonMirrorSucceeded = "MirrorSucceeded"

	// ReasonMirrorFailed indicates the internal HelmRepository create/update failed.
	ReasonMirrorFailed = "MirrorFailed"

	// ReasonInternalNotReady indicates the internal HelmRepository is not yet ready.
	ReasonInternalNotReady = "InternalNotReady"

	// ReasonInternalReady indicates the internal HelmRepository has reported Ready.
	ReasonInternalReady = "InternalReady"

	// ReasonCleanupFailed indicates deletion of the internal HelmRepository failed.
	ReasonCleanupFailed = "CleanupFailed"

	ReasonProcessing = "Processing"

	LabelManagedBy      = "helm.deckhouse.io/managed-by"
	LabelManagedByValue = "operator-helm"
	LabelSourceName     = "helm.deckhouse.io/cluster-addon"
)
