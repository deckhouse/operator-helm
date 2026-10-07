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
	HelmApplicationKind     = "HelmApplication"
	HelmApplicationResource = "helmapplications"

	// HelmApplicationLabelSourceName stores the name of the source facade resource.
	HelmApplicationLabelSourceName = "helm.deckhouse.io/application"
)

// HelmApplication describes a Helm release within a single namespace.
//
// The release is deployed in the same namespace as the HelmApplication resource.
//
// The chart is deployed using a ServiceAccount with permissions to perform any operation on all resources in the namespace. Therefore, namespace administrator permissions are required to create a HelmApplication.
//
// The Role and RoleBinding associated with the ServiceAccount are managed by the module and automatically reconciled to their desired state. Any manual changes to these resources are overwritten.
//
// +kubebuilder:object:root=true
// +crd-enricher:raw:properties.apiVersion.x-doc-skip=true
// +crd-enricher:raw:properties.kind.x-doc-skip=true
// +crd-enricher:raw:properties.metadata.x-doc-skip=true
// +kubebuilder:subresource:status
// +kubebuilder:metadata:labels={heritage=deckhouse,module=operator-helm}
// +kubebuilder:resource:singular=helmapplication,scope=Namespaced
// +kubebuilder:validation:XValidation:rule="self.metadata.name.size() <= 63",message="application name must be at most 63 characters long"
// +kubebuilder:printcolumn:name="Chart",type="string",JSONPath=".spec.chart.name",description="Helm release chart name."
// +kubebuilder:printcolumn:name="Chart Version",type="string",JSONPath=".spec.chart.version",description="Helm release chart version."
// +kubebuilder:printcolumn:name="Status",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].status",description="The readiness status of the application"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
// +kubebuilder:printcolumn:name="Repository",type="string",JSONPath=".spec.chart.repository",priority=1,description="The namespaced repository the chart is taken from"
// +kubebuilder:printcolumn:name="Cluster Repository",type="string",JSONPath=".spec.chart.clusterRepository",priority=1,description="The cluster-wide repository the chart is taken from"
// +kubebuilder:printcolumn:name="Message",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].message",priority=1
// +genclient
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type HelmApplication struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   HelmApplicationSpec   `json:"spec"`
	Status HelmApplicationStatus `json:"status,omitempty"`
}

func (r *HelmApplication) GetConditions() *[]metav1.Condition {
	return &r.Status.Conditions
}

func (r *HelmApplication) SetObservedGeneration(generation int64) {
	r.Status.ObservedGeneration = generation
}

func (r *HelmApplication) GetObservedGeneration() int64 {
	return r.Status.ObservedGeneration
}

// RepositoryName returns the name of the repository the chart is taken from,
// whichever of the two mutually exclusive reference fields is set.
func (r *HelmApplication) RepositoryName() string {
	if r.Spec.Chart.Repository != "" {
		return r.Spec.Chart.Repository
	}

	return r.Spec.Chart.ClusterRepository
}

// RepositoryKind returns the kind of the repository the chart is taken from, or an
// empty string when neither reference is set.
func (r *HelmApplication) RepositoryKind() string {
	switch {
	case r.Spec.Chart.Repository != "":
		return HelmApplicationRepositoryKind
	case r.Spec.Chart.ClusterRepository != "":
		return HelmClusterApplicationRepositoryKind
	default:
		return ""
	}
}

func (r *HelmApplication) MaintenanceModeActivated() bool {
	return r.Spec.Maintenance == string(NoResourceReconciliation)
}

func (r *HelmApplication) MaintenanceModeEnabled() bool {
	return apimeta.IsStatusConditionPresentAndEqual(r.Status.Conditions, ConditionTypeManaged, metav1.ConditionFalse)
}

func (r *HelmApplication) GetConditionTypesForUpdate() []string {
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

func (r *HelmApplication) ConfigurationApplyInProgress() bool {
	cond := apimeta.FindStatusCondition(r.Status.Conditions, ConditionTypeConfigurationApplied)
	if cond == nil {
		return false
	}

	return cond.Status == metav1.ConditionUnknown && (cond.Reason == ReasonApplyingValues || cond.Reason == ReasonReconciling)
}

func (r *HelmApplication) UpdateInstallInProgress() bool {
	cond := apimeta.FindStatusCondition(r.Status.Conditions, ConditionTypeUpdateInstalled)
	if cond == nil {
		return false
	}

	return cond.Status == metav1.ConditionUnknown && (cond.Reason == ReasonUpgrading || cond.Reason == ReasonReconciling)
}

// IsChartStatusInfoOutdated reports whether the whole chart reference, both repository
// fields included, differs from the one the release was last deployed from.
func (r *HelmApplication) IsChartStatusInfoOutdated() bool {
	if r.Status.LastAppliedChart == nil {
		return true
	}

	return r.Spec.Chart.Name != r.Status.LastAppliedChart.Name ||
		r.Spec.Chart.Repository != r.Status.LastAppliedChart.Repository ||
		r.Spec.Chart.ClusterRepository != r.Status.LastAppliedChart.ClusterRepository ||
		r.Spec.Chart.Version != r.Status.LastAppliedChart.Version
}

func (r *HelmApplication) ForceReconcileRequired() bool {
	annotations := r.GetAnnotations()
	if annotations == nil {
		return false
	}

	_, found := annotations[AnnotationForceReconcile]

	return found
}

type HelmApplicationSpec struct {
	Chart HelmApplicationChartRef `json:"chart"`
	// Custom Helm chart values.
	// +kubebuilder:pruning:PreserveUnknownFields
	// +optional
	Values *apiextensionsv1.JSON `json:"values,omitempty"`
	// Resource reconciliation mode.
	//
	// When set to `NoResourceReconciliation`, the controller pauses reconciliation of managed resources, allowing them to be modified manually without the controller overwriting the changes.
	// When set to an empty value (`""`), the standard reconciliation mode is used.
	// +kubebuilder:validation:Enum="";NoResourceReconciliation
	// +optional
	Maintenance string `json:"maintenance,omitempty"`
	// Timeout is the time to wait for a Helm action to complete. Defaults to 5m.
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:Pattern="^([0-9]+(\\.[0-9]+)?(ms|s|m|h))+$"
	// +kubebuilder:validation:XValidation:rule="duration(self) > duration('0s')",message="timeout must be greater than zero"
	// +kubebuilder:validation:XValidation:rule="duration(self) <= duration('2h')",message="timeout must not exceed 2h"
	// +optional
	Timeout *metav1.Duration `json:"timeout,omitempty"`
}

// +kubebuilder:validation:XValidation:rule="has(self.repository) != has(self.clusterRepository)",message="exactly one of spec.chart.repository or spec.chart.clusterRepository must be set"
type HelmApplicationChartRef struct {
	// Name of the Helm chart in the specified repository (for example, `nginx` or `redis`).
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
	// Name of the HelmApplicationRepository resource in the same namespace.
	//
	// The specified repository is used as the Helm chart source.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Repository string `json:"repository,omitempty"`
	// Name of the HelmClusterApplicationRepository resource.
	//
	// The specified repository is used as the Helm chart source.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	ClusterRepository string `json:"clusterRepository,omitempty"`
	// Helm chart version to install.
	// +kubebuilder:validation:MinLength=1
	Version string `json:"version"`
}

type HelmApplicationStatus struct {
	// Helm chart used during the last application installation or upgrade.
	// +optional
	LastAppliedChart *HelmApplicationLastAppliedChartRef `json:"lastAppliedChart,omitempty"`
	// Custom Helm chart values used during the last application installation or upgrade.
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
	// Inventory lists the objects the chart of the application deployed.
	// +optional
	Inventory []InventoryEntry `json:"inventory,omitempty"`
}

// HelmApplicationLastAppliedChartRef identifies the chart a HelmApplication release was last deployed from.
type HelmApplicationLastAppliedChartRef struct {
	// Name of the Helm chart used during the last application installation or upgrade.
	// +optional
	Name string `json:"name,omitempty"`
	// Name of the HelmApplicationRepository resource used during the last application installation or upgrade.
	// +optional
	Repository string `json:"repository,omitempty"`
	// Name of the HelmClusterApplicationRepository resource used during the last application installation or upgrade.
	// +optional
	ClusterRepository string `json:"clusterRepository,omitempty"`
	// Helm chart version used during the last application installation or upgrade.
	// +optional
	Version string `json:"version,omitempty"`
}

// RepositoryName returns the name of the repository the release was last deployed
// from, whichever of the two mutually exclusive reference fields is set.
func (r *HelmApplicationLastAppliedChartRef) RepositoryName() string {
	if r.Repository != "" {
		return r.Repository
	}

	return r.ClusterRepository
}

// RepositoryKind returns the kind of the repository the release was last deployed
// from, or an empty string when neither reference is set.
func (r *HelmApplicationLastAppliedChartRef) RepositoryKind() string {
	switch {
	case r.Repository != "":
		return HelmApplicationRepositoryKind
	case r.ClusterRepository != "":
		return HelmClusterApplicationRepositoryKind
	default:
		return ""
	}
}

// HelmApplicationList contains a list of HelmApplications.
// +kubebuilder:object:root=true
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type HelmApplicationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata"`

	// Items provides a list of HelmApplications.
	Items []HelmApplication `json:"items"`
}
