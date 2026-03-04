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

package helmclusteraddonrepository

import "time"

const (
	// ControllerName is the name of this controller, used for leader election and logging.
	ControllerName = "helmclusteraddonrepository-controller"

	// TargetNamespace is the namespace where internal customer resources are created.
	TargetNamespace = "d8-operator-helm"

	// FinalizerName is the finalizer added to HelmClusterRepository to ensure cleanup.
	FinalizerName = "helm.deckhouse.io/cleanup"

	// ConditionTypeReady is the condition type for readiness.
	ConditionTypeReady = "Ready"

	// ConditionTypeSynced is the condition type to track chart sync status
	ConditionTypeSynced = "Synced"

	// ReasonMirrorFailed indicates the internal HelmRepository create/update failed.
	ReasonMirrorFailed = "MirrorFailed"

	ReasonSyncSucceeded = "SyncSucceeded"

	ReasonSyncFailed = "SyncFailed"

	// ReasonInternalNotReady indicates the internal HelmRepository is not yet ready.
	ReasonInternalNotReady = "InternalNotReady"

	// ReasonInternalReady indicates the internal HelmRepository has reported Ready.
	ReasonInternalReady = "InternalReady"

	// ReasonCleanupFailed indicates deletion of the internal HelmRepository failed.
	ReasonCleanupFailed = "CleanupFailed"

	// LabelManagedBy marks an internal HelmRepository as managed by this controller.
	LabelManagedBy = "helm.deckhouse.io/managed-by"

	// LabelManagedByValue is the value for the managed-by label.
	LabelManagedByValue = "operator-helm"

	// LabelSourceName stores the name of the source HelmClusterAddonRepository.
	LabelSourceName = "helm.deckhouse.io/cluster-addon-repository"

	// DefaultInterval is the default reconciliation interval for the internal repository.
	DefaultInterval = 5 * time.Minute

	DefaultSyncInterval = 5 * time.Minute
)
