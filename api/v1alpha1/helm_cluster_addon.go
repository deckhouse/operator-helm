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

// The name is bounded to 63 characters, because it is carried as the value of the
// source-name label on every internal object derived from the addon, and a label
// value cannot be longer than that. The derived object names themselves do not
// bound it — they truncate and hash whatever they are given
// (internal/utils/name.go) — but a label value has nowhere to put the overflow.
//
// The bound is enforced by the validating webhook on CREATE
// (internal/webhook/helmclusteraddon), not by a CEL rule here. A rule declared on
// the object is evaluated on every write of the whole object, so an addon that
// already carries a longer name would fail its status updates and, because of the
// helm.deckhouse.io/cleanup finalizer, could never be deleted either.
//
// The printer columns below are the ones HelmApplication shows, minus its "Cluster
// Repository": an application picks between a namespaced and a cluster-wide
// repository, while an addon has only the one repository kind, so there is no
// second column to fill. That difference is deliberate, not an oversight.
//
// These notes are outside the doc comment on purpose: a doc comment on the type
// becomes the description of the kind in the CRD.

// HelmClusterAddon represents a single cluster-wide installation of a Helm chart, which may include custom resource definitions (CRDs) and requires cluster-admin permissions to deploy. Only one instance of a specific chart can be installed at any given time.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:metadata:labels={heritage=deckhouse,module=operator-helm}
// +kubebuilder:resource:singular=helmclusteraddon,scope=Cluster
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

	return cond.Status == metav1.ConditionUnknown && cond.Reason == ReasonReconciling
}

func (r *HelmClusterAddon) UpdateInstallInProgress() bool {
	cond := apimeta.FindStatusCondition(r.Status.Conditions, ConditionTypeUpdateInstalled)
	if cond == nil {
		return false
	}

	return cond.Status == metav1.ConditionUnknown && cond.Reason == ReasonReconciling
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
	Values *apiextensionsv1.JSON `json:"values,omitempty"`
	// Namespace to deploy cluster addon release
	// +kubebuilder:default:="default"
	// +optional
	// +kubebuilder:validation:MinLength=3
	// +kubebuilder:validation:MaxLength=63
	Namespace string `json:"namespace,omitempty"`
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
	// The minimum below is 1, not 3: the referenced kind shipped without a minimum of
	// its own, so a repository created under a shorter name must stay referenceable.
	// This note is outside the doc comment on purpose — a doc comment becomes the
	// field's description in the CRD.

	// Specifies the name of the HelmClusterAddonRepository custom resource that contains
	// the connection details and credentials for the repository where
	// the chart is located.
	// +kubebuilder:validation:MinLength=1
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
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// LastForceReconcileTime is the time the most recent force reconcile request was
	// processed. It records that the request was acted on, not that it succeeded:
	// the outcome is reported by Ready.
	// +optional
	LastForceReconcileTime *metav1.Time `json:"lastForceReconcileTime,omitempty"`
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
