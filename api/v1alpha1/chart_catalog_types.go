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

// ChartCatalogStatus and ChartVersion are shared by every chart catalog kind:
// HelmClusterAddonChart, HelmApplicationChart and HelmClusterApplicationChart are
// all projections of a repository index and differ only in scope. One declaration
// lets the controller write all three catalogs through one generic code path and
// lets chart-values-controller read them the same way.
//
// This note is outside every doc comment on purpose: a doc comment on a Status type
// becomes the description of the status field in the CRD.

type ChartCatalogStatus struct {
	// URL of the Helm chart icon.
	//
	// Applicable only to charts from Helm repositories.
	IconURL string `json:"iconURL,omitempty"`
	// Conditions reflecting the current state of the Helm chart.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// Latest resource generation processed by the controller.
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// List of discovered Helm chart versions.
	//
	// A version is available for installation if `unavailableReason` is not set.
	// For versions from an OCI repository, a supported layer media type must also be specified.
	// +optional
	Versions []ChartVersion `json:"versions"`
}

type ChartVersion struct {
	// Helm chart version.
	// +kubebuilder:validation:MinLength=1
	Version string `json:"version"`
	// OCI reference to the published Helm chart version.
	//
	// Populated only for a version from a Helm repository if the corresponding repository index entry references an OCI repository instead of a chart archive.
	// Such a version is installed using an internal OCIRepository.
	// +optional
	OCIRef string `json:"ociRef,omitempty"`
	// OCI media type of the layer containing the Helm chart version.
	//
	// Populated only for versions from an OCI repository (`oci://`) with a supported layer type.
	// If the field is not set, the version is unavailable for installation.
	// +optional
	MediaType string `json:"mediaType,omitempty"`
	// Reason why the Helm chart version is unavailable for installation.
	//
	// If the field is not set, the version is available.
	// +optional
	// +kubebuilder:validation:Enum=RemovedFromRepository;UnsupportedMediaType;ResolvePending;InvalidChartReference
	UnavailableReason string `json:"unavailableReason,omitempty"`
	// Detailed description of the reason specified in `unavailableReason`.
	// +optional
	UnavailableMessage string `json:"unavailableMessage,omitempty"`
}
