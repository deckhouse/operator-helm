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

// This note is outside every doc comment on purpose: a doc comment on a Status type becomes the description of the status field in the CRD.

type ChartCatalogStatus struct {
	// IconURL is the URL to the Helm chart icon (applicable to Helm Chart repository charts only).
	// +optional
	IconURL string `json:"iconURL,omitempty"`
	// Conditions represent the latest available observations of the chart state.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// Generation represents resource generation that was last processed by the controller.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// Versions lists every chart version the controller has examined. A version is
	// usable when it has no unavailableReason.
	// +optional
	Versions []ChartVersion `json:"versions,omitempty"`
}

type ChartVersion struct {
	// Helm chart version
	// +kubebuilder:validation:MinLength=1
	Version string `json:"version"`
	// OCIRef is the OCI reference this version is published at, as recorded from the repository index.
	// It is set only for a version of a helm repository whose index entry points at a registry instead of a chart archive.
	// +optional
	OCIRef string `json:"ociRef,omitempty"`
	// MediaType is the OCI media type of the layer that holds this chart version.
	// It is set only for a supported layer of a version of an oci:// repository.
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
	// ReexaminationRequired says the verdict recorded for this version is stale and
	// must be established again against the repository.
	// +optional
	ReexaminationRequired bool `json:"reexaminationRequired,omitempty"`
}
