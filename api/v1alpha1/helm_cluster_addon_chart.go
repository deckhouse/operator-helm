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
)

// HelmClusterAddonChart represents a Helm chart from specific repository.
//
// +kubebuilder:object:root=true
// +kubebuilder:metadata:labels={heritage=deckhouse,module=operator-helm}
// +kubebuilder:resource:categories={all,operator-helm},singular=helmclusteraddonchart,scope=Cluster
// +genclient
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type HelmClusterAddonChart struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +kubebuilder:default:={"observedGeneration":-1}
	Status HelmClusterAddonChartStatus `json:"status,omitempty"`
}

type HelmClusterAddonChartSpec struct {
	ChartName      string `json:"chartName"`
	RepositoryName string `json:"repositoryName"`
}

type HelmClusterAddonChartStatus struct {
	// Conditions represent the latest available observations of the repository state.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// +optional
	Versions []HelmClusterAddonChartVersion `json:"versions"`
}

// TODO: need to clarify what kind of information we need to render in UI for every available chart version.
// It makes sense to create Internal chart only during the first application deploy.

type HelmClusterAddonChartVersion struct {
	Version string `json:"version"`
	Digest  string `json:"digest"`
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
