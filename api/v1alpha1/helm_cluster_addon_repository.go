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
	HelmClusterAddonRepositoryKind     = "HelmClusterAddonRepository"
	HelmClusterAddonRepositoryResource = "helmclusteraddonrepositories"

	// HelmClusterAddonRepositoryLabelSourceName stores the name of the source facade resource.
	HelmClusterAddonRepositoryLabelSourceName = "helm.deckhouse.io/cluster-addon-repository"
)

// HelmClusterAddonRepository represents a Helm or OCI-compliant repository containing Helm charts that can be referenced by HelmClusterAddon resources.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:metadata:labels={heritage=deckhouse,module=operator-helm}
// +kubebuilder:resource:singular=helmclusteraddonrepository,scope=Cluster
// +kubebuilder:printcolumn:name="Status",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].status",description="The readiness status of the repository"
// +kubebuilder:printcolumn:name="Synced",type="string",JSONPath=".status.conditions[?(@.type=='Synced')].status",description="Repository synchronization status"
// +kubebuilder:printcolumn:name="Last Sync",type="date",JSONPath=".status.lastSuccessfulSyncTime",description="Time of the last successful catalog synchronization"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
// +kubebuilder:printcolumn:name="Next Sync",type="date",JSONPath=".status.nextSyncTime",priority=1,description="Scheduled time of the next synchronization attempt"
// +kubebuilder:printcolumn:name="Message",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].message",priority=1
// +genclient
// +genclient:nonNamespaced
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type HelmClusterAddonRepository struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   HelmClusterAddonRepositorySpec   `json:"spec"`
	Status HelmClusterAddonRepositoryStatus `json:"status,omitempty"`
}

func (r *HelmClusterAddonRepository) GetConditions() *[]metav1.Condition {
	return &r.Status.Conditions
}

func (r *HelmClusterAddonRepository) SetObservedGeneration(generation int64) {
	r.Status.ObservedGeneration = generation
}

func (r *HelmClusterAddonRepository) GetObservedGeneration() int64 {
	return r.Status.ObservedGeneration
}

func (r *HelmClusterAddonRepository) GetStatus() any {
	return r.Status
}

func (r *HelmClusterAddonRepository) GetConditionTypesForUpdate() []string {
	return []string{"Ready"}
}

func (r *HelmClusterAddonRepository) ForceReconcileRequired() bool {
	annotations := r.GetAnnotations()
	if annotations == nil {
		return false
	}

	_, found := annotations[AnnotationForceReconcile]

	return found
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

	// InsecureSkipVerify disable TLS certificate verification.
	// +optional
	InsecureSkipVerify bool `json:"insecureSkipVerify,omitempty"`
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
	//
	// Ready reports whether the repository is usable: auxiliary resources are in place,
	// the internal source object is healthy and the repository has responded to a catalog
	// read on the current spec. A transient read failure does not flip Ready to False.
	//
	// Synced reports whether the chart catalog is up to date.
	//
	// Reconciling and Stalled follow the kstatus convention: they are present only while
	// applicable. Reconciling means work is in progress or a retry is scheduled; Stalled
	// means the repository will not recover without a change.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// Generation represents resource generation that was last processed by the controller.
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// LastSuccessfulSyncTime is the last time the chart catalog was fully brought up to date,
	// including creating and pruning chart resources.
	// +optional
	LastSuccessfulSyncTime *metav1.Time `json:"lastSuccessfulSyncTime,omitempty"`
	// NextSyncTime is the scheduled time of the next synchronization attempt.
	// +optional
	NextSyncTime *metav1.Time `json:"nextSyncTime,omitempty"`
	// ConsecutiveFetchFailures counts consecutive failures to read from the repository.
	// It drives the retry backoff and resets on the first success.
	// +optional
	ConsecutiveFetchFailures int32 `json:"consecutiveFetchFailures,omitempty"`
}

// HelmClusterAddonRepositoryList contains a list of HelmClusterAddonRepositories.
// +kubebuilder:object:root=true
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type HelmClusterAddonRepositoryList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata"`

	// Items provides a list of HelmClusterAddonRepositories.
	Items []HelmClusterAddonRepository `json:"items"`
}
