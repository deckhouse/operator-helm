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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	HelmClusterAddonChartKind     = "HelmClusterAddonChart"
	HelmClusterAddonChartResource = "helmclusteraddoncharts"

	HelmClusterAddonChartLabelSourceName = "helm.deckhouse.io/cluster-addon-chart"
)

// HelmClusterAddonChart represents a Helm chart and its versions from specific repository.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:metadata:labels={heritage=deckhouse,module=operator-helm}
// +kubebuilder:resource:categories={all,operator-helm},singular=helmclusteraddonchart,scope=Cluster
// +genclient
// +genclient:nonNamespaced
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type HelmClusterAddonChart struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Status HelmClusterAddonChartStatus `json:"status,omitempty"`
}

func (r *HelmClusterAddonChart) GetConditions() *[]metav1.Condition {
	return &r.Status.Conditions
}

func (r *HelmClusterAddonChart) SetObservedGeneration(generation int64) {
	r.Status.ObservedGeneration = generation
}

func (r *HelmClusterAddonChart) GetObservedGeneration() int64 {
	return r.Status.ObservedGeneration
}

func (r *HelmClusterAddonChart) GetStatus() any {
	return r.Status
}

func (r *HelmClusterAddonChart) GetConditionTypesForUpdate() []string {
	return []string{"Ready"}
}

type HelmClusterAddonChartSpec struct {
	// Helm chart name
	// +kubebuilder:validation:MinLength=1
	ChartName string `json:"chartName"`
	// Name of HelmClusterAddonRepository where respective helm chart resides.
	// +kubebuilder:validation:MinLength=3
	// +kubebuilder:validation:MaxLength=63
	RepositoryName string `json:"repositoryName"`
}

type HelmClusterAddonChartStatus struct {
	// Conditions represent the latest available observations of the repository state.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// Generating a resource that was last processed by the controller.
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// Available helm chart versions
	// +optional
	Versions []HelmClusterAddonChartVersion `json:"versions"`
}

type HelmClusterAddonChartVersion struct {
	// Helm chart version
	// +kubebuilder:validation:MinLength=1
	Version string `json:"version"`
	// Helm chart digest
	Digest string `json:"digest,omitempty"`
	// Chart pulled from repository
	Pulled bool `json:"pulled"`
}

// HelmClusterAddonChartList contains a list of HelmClusterAddonCharts.
// +kubebuilder:object:root=true
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type HelmClusterAddonChartList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata"`

	// Items provides a list of HelmClusterAddonCharts.
	Items []HelmClusterAddonChart `json:"items"`
}
