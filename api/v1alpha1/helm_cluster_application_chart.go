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
	HelmClusterApplicationChartKind     = "HelmClusterApplicationChart"
	HelmClusterApplicationChartResource = "helmclusterapplicationcharts"

	HelmClusterApplicationChartLabelSourceName = "helm.deckhouse.io/cluster-application-chart"
)

// The status of this kind is the shared ChartCatalogStatus declared in
// chart_catalog_types.go: every chart catalog kind of the module has the same shape.
//
// The object carries no spec on purpose: it is a projection of a repository catalog,
// not user input. Writes by anyone other than the module's service accounts are
// refused by the ValidatingAdmissionPolicy in templates/admision-policy.yaml.
//
// These notes are deliberately outside the doc comment below — controller-gen folds
// every non-marker line of that block into the resource's API description.

// HelmClusterApplicationChart represents a specific Helm chart discovered within a HelmClusterApplicationRepository. These resources are automatically managed during repository synchronization and are immutable to user modifications.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:metadata:labels={heritage=deckhouse,module=operator-helm}
// +kubebuilder:resource:singular=helmclusterapplicationchart,scope=Cluster
// +genclient
// +genclient:nonNamespaced
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type HelmClusterApplicationChart struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Status ChartCatalogStatus `json:"status,omitempty"`
}

func (r *HelmClusterApplicationChart) GetConditions() *[]metav1.Condition {
	return &r.Status.Conditions
}

func (r *HelmClusterApplicationChart) SetObservedGeneration(generation int64) {
	r.Status.ObservedGeneration = generation
}

func (r *HelmClusterApplicationChart) GetObservedGeneration() int64 {
	return r.Status.ObservedGeneration
}

func (r *HelmClusterApplicationChart) GetStatus() any {
	return r.Status
}

func (r *HelmClusterApplicationChart) GetConditionTypesForUpdate() []string {
	return []string{ConditionTypeReady}
}

// HelmClusterApplicationChartList contains a list of HelmClusterApplicationCharts.
// +kubebuilder:object:root=true
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type HelmClusterApplicationChartList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata"`

	// Items provides a list of HelmClusterApplicationCharts.
	Items []HelmClusterApplicationChart `json:"items"`
}
