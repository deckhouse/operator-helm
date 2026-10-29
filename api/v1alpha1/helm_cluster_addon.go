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

import (
	"reflect"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	HelmClusterAddonKind     = "HelmClusterAddon"
	HelmClusterAddonResource = "helmclusteraddons"

	// LabelSourceName stores the name of the source facade resource.
	HelmClusterAddonLabelSourceName = "helm.deckhouse.io/cluster-addon"
)

// HelmClusterAddon describes a Helm release managed at the cluster level.
//
// The Helm chart may contain CRDs and other cluster-wide resources, so `SuperAdmin` permissions are required to create a HelmClusterAddon.
// Only one HelmClusterAddon resource can exist in the cluster for a given Helm chart from a specific repository.
//
// +kubebuilder:object:root=true
// +crd-enricher:raw:properties.apiVersion.x-doc-skip=true
// +crd-enricher:raw:properties.kind.x-doc-skip=true
// +crd-enricher:raw:properties.metadata.x-doc-skip=true
// +kubebuilder:subresource:status
// +kubebuilder:metadata:labels={heritage=deckhouse,module=operator-helm}
// +kubebuilder:resource:singular=helmclusteraddon,scope=Cluster
// +kubebuilder:validation:XValidation:rule="self.metadata.name.size() <= 63",message="addon name must be at most 63 characters long"
// +kubebuilder:printcolumn:name="Chart",type="string",JSONPath=".spec.chart.helmClusterAddonChart",description="Helm release chart name."
// +kubebuilder:printcolumn:name="Chart Version",type="string",JSONPath=".spec.chart.version",description="Helm release chart version."
// +kubebuilder:printcolumn:name="Status",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].status",description="The readiness status of the addon"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
// +kubebuilder:printcolumn:name="Repository",type="string",JSONPath=".spec.chart.helmClusterAddonRepository",priority=1,description="The repository the chart is taken from"
// +kubebuilder:printcolumn:name="Message",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].message",priority=1
// +genclient
// +genclient:nonNamespaced
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type HelmClusterAddon struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   HelmClusterAddonSpec   `json:"spec"`
	Status HelmClusterAddonStatus `json:"status,omitempty"`
}

func (r *HelmClusterAddon) GetConditions() *[]metav1.Condition {
	return &r.Status.Conditions
}

func (r *HelmClusterAddon) SetObservedGeneration(generation int64) {
	r.Status.ObservedGeneration = generation
}

func (r *HelmClusterAddon) GetObservedGeneration() int64 {
	return r.Status.ObservedGeneration
}

func (r *HelmClusterAddon) MaintenanceModeActivated() bool {
	return r.Spec.Maintenance == string(NoResourceReconciliation)
}

func (r *HelmClusterAddon) MaintenanceModeEnabled() bool {
	return apimeta.IsStatusConditionPresentAndEqual(r.Status.Conditions, ConditionTypeManaged, metav1.ConditionFalse)
}

func (r *HelmClusterAddon) GetConditionTypesForUpdate() []string {
	conditionTypes := []string{ConditionTypeReady}

	if r.Status.LastAppliedChart == nil || !apimeta.IsStatusConditionPresentAndEqual(r.Status.Conditions, ConditionTypeInstalled, metav1.ConditionTrue) {
		return append(conditionTypes, ConditionTypeInstalled)
	}

	if r.IsChartStatusInfoOutdated() ||
		apimeta.IsStatusConditionFalse(r.Status.Conditions, ConditionTypeUpdateInstalled) ||
		r.UpdateInstallInProgress() {
		conditionTypes = append(conditionTypes, ConditionTypeUpdateInstalled)
	}

	if !reflect.DeepEqual(r.Spec.Values, r.Status.LastAppliedValues) ||
		apimeta.IsStatusConditionFalse(r.Status.Conditions, ConditionTypeConfigurationApplied) ||
		r.ConfigurationApplyInProgress() {
		conditionTypes = append(conditionTypes, ConditionTypeConfigurationApplied)
	}

	return conditionTypes
}

func (r *HelmClusterAddon) ConfigurationApplyInProgress() bool {
	cond := apimeta.FindStatusCondition(r.Status.Conditions, ConditionTypeConfigurationApplied)
	if cond == nil {
		return false
	}

	return cond.Status == metav1.ConditionUnknown && (cond.Reason == ReasonApplyingValues || cond.Reason == ReasonReconciling)
}

func (r *HelmClusterAddon) UpdateInstallInProgress() bool {
	cond := apimeta.FindStatusCondition(r.Status.Conditions, ConditionTypeUpdateInstalled)
	if cond == nil {
		return false
	}

	return cond.Status == metav1.ConditionUnknown && (cond.Reason == ReasonUpgrading || cond.Reason == ReasonReconciling)
}

func (r *HelmClusterAddon) IsChartStatusInfoOutdated() bool {
	if r.Status.LastAppliedChart == nil {
		return true
	}

	return r.Spec.Chart.HelmClusterAddonChartName != r.Status.LastAppliedChart.HelmClusterAddonChartName ||
		r.Spec.Chart.HelmClusterAddonRepository != r.Status.LastAppliedChart.HelmClusterAddonRepository ||
		r.Spec.Chart.Version != r.Status.LastAppliedChart.Version
}

func (r *HelmClusterAddon) ForceReconcileRequired() bool {
	annotations := r.GetAnnotations()
	if annotations == nil {
		return false
	}

	_, found := annotations[AnnotationForceReconcile]

	return found
}

type HelmClusterAddonSpec struct {
	Chart HelmClusterAddonChartRef `json:"chart"`
	// Custom Helm chart values.
	// +kubebuilder:pruning:PreserveUnknownFields
	// +optional
	Values *apiextensionsv1.JSON `json:"values,omitempty"`
	// Namespace to deploy a cluster addon release into.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Namespace string `json:"namespace"`
	// Resource reconciliation mode.
	//
	// When set to `NoResourceReconciliation`, the controller pauses reconciliation of managed resources, allowing them to be modified manually without the controller overwriting the changes.
	// When set to an empty value (`""`), the standard reconciliation mode is used.
	// +kubebuilder:validation:Enum="";NoResourceReconciliation
	// +optional
	Maintenance string `json:"maintenance,omitempty"`
	// Time to wait for a Helm operation to complete.
	//
	// Default: `5m`. The value must be greater than zero and must not exceed `2h`.
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:Pattern="^([0-9]+(\\.[0-9]+)?(ms|s|m|h))+$"
	// +kubebuilder:validation:XValidation:rule="duration(self) > duration('0s')",message="timeout must be greater than zero"
	// +kubebuilder:validation:XValidation:rule="duration(self) <= duration('2h')",message="timeout must not exceed 2h"
	// +optional
	Timeout *metav1.Duration `json:"timeout,omitempty"`
}

type HelmClusterAddonChartRef struct {
	// Name of the Helm chart in the specified repository (for example, `ingress-nginx` or `redis`).
	// +kubebuilder:validation:MinLength=1
	HelmClusterAddonChartName string `json:"helmClusterAddonChart"`
	// Name of the HelmClusterAddonRepository resource.
	//
	// The specified repository is used as the Helm chart source.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	HelmClusterAddonRepository string `json:"helmClusterAddonRepository"`
	// Helm chart version to install.
	Version string `json:"version"`
}

type HelmClusterAddonStatus struct {
	// Helm chart used during the last addon installation or upgrade.
	// +optional
	LastAppliedChart *HelmClusterAddonLastAppliedChartRef `json:"lastAppliedChart,omitempty"`
	// Custom Helm chart values used during the last addon installation or upgrade.
	// +optional
	LastAppliedValues *apiextensionsv1.JSON `json:"lastAppliedValues,omitempty"`
	// Conditions reflecting the current state of the resource.
	// +optional
	// +crd-enricher:raw:items.properties.lastTransitionTime.description=Time when the condition status last changed.
	// +crd-enricher:raw:items.properties.message.description=Message with additional information about the condition state.
	// +crd-enricher:raw:items.properties.observedGeneration.description=Resource generation on which the condition state is based.
	// +crd-enricher:raw:items.properties.reason.description=Reason for the last change in the condition state.
	// +crd-enricher:raw:items.properties.status.description=Condition status.
	// +crd-enricher:raw:items.properties.type.description=Condition type.
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// Latest resource generation processed by the controller.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// Time when the last forced reconciliation request was processed.
	//
	// This value indicates that the request was processed but does not indicate that reconciliation completed successfully.
	// Reconciliation results are reflected in the `Ready` condition.
	// +optional
	LastForceReconcileTime *metav1.Time `json:"lastForceReconcileTime,omitempty"`
	// List of objects deployed by the addon's Helm chart.
	// +optional
	Inventory []InventoryEntry `json:"inventory,omitempty"`
}

type HelmClusterAddonLastAppliedChartRef struct {
	// Helm chart name.
	// +optional
	HelmClusterAddonChartName string `json:"helmClusterAddonChart,omitempty"`
	// Name of the HelmClusterAddonRepository resource.
	// +optional
	HelmClusterAddonRepository string `json:"helmClusterAddonRepository,omitempty"`
	// Helm chart version.
	// +optional
	Version string `json:"version,omitempty"`
}

// HelmClusterAddonList contains a list of HelmClusterAddons.
// +kubebuilder:object:root=true
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type HelmClusterAddonList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata"`

	// Items provides a list of HelmClusterAddons.
	Items []HelmClusterAddon `json:"items"`
}

// HelmClusterAddonMaintenance describe HelmClusterAddon maintenance operation mode.
// +kubebuilder:validation:Enum={"",NoResourceReconciliation}
type HelmClusterAddonMaintenance string

const (
	NoResourceReconciliation HelmClusterAddonMaintenance = "NoResourceReconciliation"
)
