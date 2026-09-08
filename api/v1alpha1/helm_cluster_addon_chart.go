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

	// UnavailableReason* are the values of HelmClusterAddonChartVersion.UnavailableReason.
	// They are field values rather than condition reasons, so they live next to the
	// type that carries them instead of conditions.go.
	//
	// UnavailableReasonRemovedFromRepository means the tag is no longer offered by the
	// repository. The entry is retained only because an addon still references it, and
	// the marker is dropped automatically once the tag is listed again.
	UnavailableReasonRemovedFromRepository = "RemovedFromRepository"
	// UnavailableReasonUnsupportedMediaType means the manifest was read but the artifact
	// is not a packaged Helm chart. It is a verdict about the artifact, so it is kept
	// until a force reconcile re-examines every tag.
	UnavailableReasonUnsupportedMediaType = "UnsupportedMediaType"
	// UnavailableReasonResolvePending means the manifest request failed and no verdict
	// was reached. Such a tag is re-examined on every normal synchronization.
	UnavailableReasonResolvePending = "ResolvePending"
	// UnavailableReasonInvalidChartReference means the repository index points this
	// version at a registry, but the reference it gives is not a valid tagged
	// reference. It is a verdict about the index entry rather than about the
	// artifact, so it is kept until the repository publishes a usable reference.
	UnavailableReasonInvalidChartReference = "InvalidChartReference"
)

// HelmClusterAddonChart represents a specific Helm chart discovered within a HelmClusterAddonRepository. These resources are automatically managed during repository synchronization and are immutable to user modifications.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:metadata:labels={heritage=deckhouse,module=operator-helm}
// +kubebuilder:resource:singular=helmclusteraddonchart,scope=Cluster
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

type HelmClusterAddonChartStatus struct {
	// IconURL is the URL to the Helm chart icon (applicable to Helm Chart repository charts only).
	IconURL string `json:"iconURL,omitempty"`
	// Conditions represent the latest available observations of the addon chart state.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// Generation represents resource generation that was last processed by the controller.
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// Versions lists every chart version the controller has examined. A version is
	// usable when it has no unavailableReason; for an OCI repository a usable version
	// also carries the media type of the layer that holds it.
	// +optional
	Versions []HelmClusterAddonChartVersion `json:"versions"`
}

type HelmClusterAddonChartVersion struct {
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

// HelmClusterAddonChartList contains a list of HelmClusterAddonCharts.
// +kubebuilder:object:root=true
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type HelmClusterAddonChartList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata"`

	// Items provides a list of HelmClusterAddonCharts.
	Items []HelmClusterAddonChart `json:"items"`
}
