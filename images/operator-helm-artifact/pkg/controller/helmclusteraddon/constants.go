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

import "time"

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
	ReasonUpdateSucceeded        = "UpdateSucceeded"
	ReasonInstallSucceeded       = "InstallSucceeded"
	ReasonInstallationInProgress = "InstallationInProgress"
	ReasonUpdateInProgress       = "UpdateInProgress"
	ReasonInstallFailed          = "InstallFailed"
	ReasonUpdateFailed           = "UpdateFailed"

	// ReasonProcessing indicates that facade resource is processing.
	ReasonProcessing = "Processing"

	// ReasonReconcileFailed indicates a terminal error occurred during the reconcile pipeline.
	ReasonReconcileFailed = "ReconcileFailed"

	// LabelManagedBy marks resources as managed by this controller.
	LabelManagedBy = "helm.deckhouse.io/managed-by"

	// LabelManagedByValue is the value for the managed-by label.
	LabelManagedByValue = "operator-helm"

	// LabelSourceName stores the name of the source facade resource.
	LabelSourceName = "helm.deckhouse.io/cluster-addon"

	// InternalHelmReleaseDeployed indicates that specific release fon internal chart release history was deployed
	InternalHelmReleaseDeployed = "deployed"

	// ReconcileRetryInterval is the default requeue interval when waiting for non-terminal
	// states such as HelmRelease reaching a final condition.
	ReconcileRetryInterval = 5 * time.Second
)
