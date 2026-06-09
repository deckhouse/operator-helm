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
	"k8s.io/apimachinery/pkg/api/meta"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	HelmClusterAddonKind     = "HelmClusterAddon"
	HelmClusterAddonResource = "helmclusteraddons"

	// LabelSourceName stores the name of the source facade resource.
	HelmClusterAddonLabelSourceName = "helm.deckhouse.io/cluster-addon"
)

// HelmClusterAddon represents a single cluster-wide installation of a Helm chart, which may include custom resource definitions (CRDs) and requires cluster-admin permissions to deploy. Only one instance of a specific chart can be installed at any given time.

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:metadata:labels={heritage=deckhouse,module=operator-helm}
// +kubebuilder:resource:singular=helmclusteraddon,scope=Cluster
// +kubebuilder:printcolumn:name="Chart Name",type="string",JSONPath=".spec.chart.helmClusterAddonChart",description="Helm release chart name."
// +kubebuilder:printcolumn:name="Chart Version",type="string",JSONPath=".spec.chart.version",description="Helm release chart version."
// +kubebuilder:printcolumn:name="Status",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].status",description="The readiness status of the addon"
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

func (r *HelmClusterAddon) GetStatus() any {
	return r.Status
}

func (r *HelmClusterAddon) MaintenanceModeActivated() bool {
	return r.Spec.Maintenance == string(NoResourceReconciliation)
}

func (r *HelmClusterAddon) MaintenanceModeEnabled() bool {
	return apimeta.IsStatusConditionPresentAndEqual(r.Status.Conditions, ConditionTypeManaged, metav1.ConditionFalse)
}

func (r *HelmClusterAddon) GetConditionTypesForUpdate() []string {
	conditionTypes := []string{"Ready"}

	if r.Status.LastAppliedChart == nil || !meta.IsStatusConditionPresentAndEqual(r.Status.Conditions, ConditionTypeInstalled, metav1.ConditionTrue) {
		return append(conditionTypes, ConditionTypeInstalled)
	}

	if r.IsChartStatusInfoOutdated() ||
		meta.IsStatusConditionFalse(r.Status.Conditions, ConditionTypeUpdateInstalled) ||
		r.ConfigurationApplyInProgress() {
		conditionTypes = append(conditionTypes, ConditionTypeUpdateInstalled)
	}

	if !reflect.DeepEqual(r.Spec.Values, r.Status.LastAppliedValues) ||
		meta.IsStatusConditionFalse(r.Status.Conditions, ConditionTypeConfigurationApplied) ||
		r.UpdateInstallInProgress() {
		conditionTypes = append(conditionTypes, ConditionTypeConfigurationApplied)
	}

	return conditionTypes
}

func (r *HelmClusterAddon) ConfigurationApplyInProgress() bool {
	cond := meta.FindStatusCondition(r.Status.Conditions, ConditionTypeConfigurationApplied)
	if cond == nil {
		return false
	}

	return cond.Status == metav1.ConditionUnknown && cond.Reason == "Reconciling"
}

func (r *HelmClusterAddon) UpdateInstallInProgress() bool {
	cond := meta.FindStatusCondition(r.Status.Conditions, ConditionTypeUpdateInstalled)
	if cond == nil {
		return false
	}

	return cond.Status == metav1.ConditionUnknown && cond.Reason == "Reconciling"
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
	// Values holds the values for this HelmClusterAddon release.
	// +kubebuilder:pruning:PreserveUnknownFields
	// +optional
	Values *apiextensionsv1.JSON `json:"values"`
	// Namespace to deploy cluster addon release
	// +kubebuilder:default:="default"
	// +optional
	// +kubebuilder:validation:MinLength=3
	// +kubebuilder:validation:MaxLength=63
	Namespace string `json:"namespace"`
	// Maintenance specifies the reconciliation strategy for the resource.
	// When set to "NoResourceReconciliation", the controller will stop updating the
	// underlying resources, allowing for manual intervention or maintenance
	// without the operator overwriting changes.
	// When empty (""), standard reconciliation is active.
	// +kubebuilder:validation:Enum="";NoResourceReconciliation
	// +optional
	Maintenance string `json:"maintenance,omitempty"`
}

type HelmClusterAddonChartRef struct {
	// Specifies the name of the Helm chart to be installed
	// from the defined repository (e.g., "ingress-nginx" or "redis").
	// +kubebuilder:validation:MinLength=1
	HelmClusterAddonChartName string `json:"helmClusterAddonChart"`
	// Specifies the name of the HelmClusterAddonRepository custom resource that contains
	// the connection details and credentials for the repository where
	// the chart is located.
	// +kubebuilder:validation:MinLength=3
	// +kubebuilder:validation:MaxLength=63
	HelmClusterAddonRepository string `json:"helmClusterAddonRepository"`
	// Versions holds the HelmClusterAddon chart version.
	Version string `json:"version"`
}

type HelmClusterAddonStatus struct {
	// LastAppliedChart represents the latest chart that triggered addon install or update.
	// +optional
	LastAppliedChart *HelmClusterAddonLastAppliedChartRef `json:"lastAppliedChart,omitempty"`
	// LastAppliedValues represents the latest values that triggered addon install or update.
	// +optional
	LastAppliedValues *apiextensionsv1.JSON `json:"lastAppliedValues,omitempty"`
	// Conditions represent the latest available observations of the addon state.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// Generation represents resource generation that was last processed by the controller.
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

type HelmClusterAddonLastAppliedChartRef struct {
	// Specifies the name of the Helm chart to be installed
	// from the defined repository (e.g., "ingress-nginx" or "redis").
	// +optional
	HelmClusterAddonChartName string `json:"helmClusterAddonChart,omitempty"`
	// Specifies the name of the HelmClusterAddonRepository custom resource that contains
	// the connection details and credentials for the repository where
	// the chart is located.
	// +optional
	HelmClusterAddonRepository string `json:"helmClusterAddonRepository,omitempty"`
	// Versions holds the HelmClusterAddon chart version.
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
