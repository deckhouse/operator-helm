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
	HelmClusterAddonRepostoryKind      = "HelmClusterAddonRepository"
	HelmClusterAddonRepositoryResource = "helmclusteraddonrepositories"
)

// HelmClusterAddonRepository represens a Git, Helm or OCI complient repocitory with Helm charts.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:metadata:labels={heritage=deckhouse,module=operator-helm}
// +kubebuilder:resource:categories={all,operator-helm},singular=helmclusteraddonrepository,scope=Cluster
// +kubebuilder:printcolumn:name="Status",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].status",description="The readiness status of the repository"
// +genclient
// +genclient:nonNamespaced
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type HelmClusterAddonRepository struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   HelmClusterAddonRepositorySpec   `json:"spec"`
	Status HelmClusterAddonRepositoryStatus `json:"status,omitempty"`
}

type HelmClusterAddonRepositorySpec struct {
	// URL of the Helm repository. Supports http(s):// and oci:// protocols.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:XValidation:rule="self.matches('^(https?|oci)://.+$')",message="URL must have a valid protocol (http, https, oci) and a non-empty path"
	URL string `json:"url"`

	// Auth contains authentication credentials for the repository.
	// +optional
	Auth *HelmClusterAddonRepositoryAuth `json:"auth,omitempty"`

	// CACertificate is the PEM encoded CA certificate for TLS verification.
	// +optional
	CACertificate string `json:"caCertificate,omitempty"`

	// TLSVerify enables or disables TLS certificate verification.
	// +kubebuilder:default=true
	// +optional
	TLSVerify bool `json:"tlsVerify,omitempty"`
}

type HelmClusterAddonRepositoryAuth struct {
	// Repository authentication username.
	// +kubebuilder:validation:MinLength=1
	Username string `json:"username"`
	// Repository authentication password.
	// +kubebuilder:validation:MinLength=1
	Password string `json:"password"`
}

type HelmClusterAddonRepositoryStatus struct {
	// Conditions represent the latest available observations of the repository state.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// Generating a resource that was last processed by the controller.
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// HelmClusterAddonRepositoryList contains a list of HelmClusterRepositories.
// +kubebuilder:object:root=true
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type HelmClusterAddonRepositoryList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata"`

	// Items provides a list of HelmClusterRepositories.
	Items []HelmClusterAddonRepository `json:"items"`
}
