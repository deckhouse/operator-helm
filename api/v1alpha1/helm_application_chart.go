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
	HelmApplicationChartKind     = "HelmApplicationChart"
	HelmApplicationChartResource = "helmapplicationcharts"

	HelmApplicationChartLabelSourceName = "helm.deckhouse.io/application-chart"
)

// The object carries no spec on purpose: it is a projection of a repository catalog,
// not user input. Writes by anyone other than the module's service accounts are
// refused by the ValidatingAdmissionPolicy in templates/admision-policy.yaml.
//
// This note is deliberately outside the doc comment below — controller-gen folds
// every non-marker line of that block into the resource's API description.

// HelmApplicationChart represents a specific Helm chart discovered within a HelmApplicationRepository. These resources are automatically managed during repository synchronization and are immutable to user modifications.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:metadata:labels={heritage=deckhouse,module=operator-helm}
// +kubebuilder:resource:singular=helmapplicationchart,scope=Namespaced
// +genclient
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type HelmApplicationChart struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Status ApplicationChartStatus `json:"status,omitempty"`
}

func (r *HelmApplicationChart) GetConditions() *[]metav1.Condition {
	return &r.Status.Conditions
}

func (r *HelmApplicationChart) SetObservedGeneration(generation int64) {
	r.Status.ObservedGeneration = generation
}

func (r *HelmApplicationChart) GetObservedGeneration() int64 {
	return r.Status.ObservedGeneration
}

func (r *HelmApplicationChart) GetStatus() any {
	return r.Status
}

func (r *HelmApplicationChart) GetConditionTypesForUpdate() []string {
	return []string{ConditionTypeReady}
}

// ApplicationChartStatus and ApplicationChartVersion below are shared by
// HelmApplicationChart and HelmClusterApplicationChart: the two kinds differ only
// in scope. Declaring them once makes a divergence between the two schemas
// impossible by construction, and keeps a single translation for both in
// crds/doc-ru-*.yaml.
//
// This note is outside every doc comment on purpose: a doc comment on a Status type
// becomes the description of the status field in the CRD.

type ApplicationChartStatus struct {
	// IconURL is the URL to the Helm chart icon (applicable to Helm Chart repository charts only).
	IconURL string `json:"iconURL,omitempty"`
	// Conditions represent the latest available observations of the chart state.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// Generation represents resource generation that was last processed by the controller.
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// Versions lists every chart version the controller has examined. A version is
	// usable when it has no unavailableReason; for an OCI repository a usable version
	// also carries the media type of the layer that holds it.
	// +optional
	Versions []ApplicationChartVersion `json:"versions"`
}

type ApplicationChartVersion struct {
	// Helm chart version
	// +kubebuilder:validation:MinLength=1
	Version string `json:"version"`
	// OCIRef is the OCI reference this version is published at, as recorded from
	// the repository index. It is set only for a version of a helm repository whose
	// index entry points at a registry instead of a chart archive; such a version is
	// deployed through an internal OCIRepository even though its repository is a helm
	// one. The registry host and path keep the spelling the index used, and the tag is
	// always explicit: an index entry without one is recorded with its own version as
	// the tag.
	// +optional
	OCIRef string `json:"ociRef,omitempty"`
	// MediaType is the OCI media type of the layer that holds this chart version. It
	// is set only for a version of an oci:// repository, and only when the layer is
	// supported: an empty value there means the version cannot be deployed. It stays
	// empty for a version carrying OCIRef — the layer of such an artifact is examined
	// at deploy time and is not recorded here.
	// +optional
	MediaType string `json:"mediaType,omitempty"`
	// UnavailableReason explains why this version cannot be deployed. Its absence means
	// the version is usable.
	// +optional
	// +kubebuilder:validation:Enum=RemovedFromRepository;UnsupportedMediaType;ResolvePending;InvalidChartReference
	UnavailableReason string `json:"unavailableReason,omitempty"`
	// UnavailableMessage carries human readable detail for UnavailableReason.
	// +optional
	UnavailableMessage string `json:"unavailableMessage,omitempty"`
}

// HelmApplicationChartList contains a list of HelmApplicationCharts.
// +kubebuilder:object:root=true
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type HelmApplicationChartList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata"`

	// Items provides a list of HelmApplicationCharts.
	Items []HelmApplicationChart `json:"items"`
}
