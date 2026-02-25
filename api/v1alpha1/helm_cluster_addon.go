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
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	HelmClusterAddonKind     = "HelmClusterAddon"
	HelmClusterAddonResource = "helmclusteraddons"
)

// HelmClusterAddon represents a Helm addon that is installed across the whole cluster.
//
// +kubebuilder:object:root=true
// +kubebuilder:metadata:labels={heritage=deckhouse,module=operator-helm}
// +kubebuilder:resource:categories={all,operator-helm},singular=helmclusteraddon,scope=Cluster
// +kubebuilder:printcolumn:name="Chart Name",type="string",JSONPath=".spec.chart.helmClusterAddonChart",description="Helm release chart name."
// +kubebuilder:printcolumn:name="Chart Version",type="string",JSONPath=".spec.chart.version",description="Helm release chart version."
// +kubebuilder:printcolumn:name="Status",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].status",description="The readiness status of the repository"
// +genclient
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type HelmClusterAddon struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec HelmClusterAddonSpec `json:"spec"`
	// +kubebuilder:default:={"observedGeneration":-1}
	Status HelmClusterAddonStatus `json:"status,omitempty"`
}

type HelmClusterAddonSpec struct {
	Chart HelmClusterAddonChartRef `json:"chart"`
	// Values holds the values for this Helm release.
	// +kubebuilder:pruning:PreserveUnknownFields
	// +optional
	Values *apiextensionsv1.JSON `json:"values"`
	// +kubebuilder:default:="default"
	// +optional
	Namespace string `json:"namespace"`
	// +kubebuilder:validation:Enum="";NoResourceReconciliation
	// +optional
	Maintanace string `json:"maintanace,omitempty"`
}

type HelmClusterAddonChartRef struct {
	HelmClusterAddonRepository string `json:"helmClusterAddonRepository"`
	HelmClusterAddonChartName  string `json:"helmClusterAddonChart"`
	// Versions holds the Chart version.
	// +optional
	Version string `json:"version,omitempty"`
}

type HelmClusterAddonStatus struct {
	// Conditions represent the latest available observations of the repository state.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// Generating a resource that was last processed by the controller.
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
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

// HelmClusterAddonMaintanace describe HelmClusterAddon maintanance operation mode.
// +kubebuilder:validation:Enum={"",NoResourceReconciliation}
type HelmClusterAddonMaintanace string

const (
	NoResourceReconciliation HelmClusterAddonMaintanace = "NoResourceReconciliation"
)
