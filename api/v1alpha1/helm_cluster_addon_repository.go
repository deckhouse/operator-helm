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

// The name rule below is only the upper half of the one the application repositories
// carry. This kind shipped without any rule, and a root rule is re-evaluated on every
// write to the object — a status patch included — so a minimum added now would not
// merely forbid new short names, it would freeze every repository already created with
// one, with no way out but deleting it and its whole catalog. The cap is different: a
// name longer than a label value can hold already breaks the internal objects that
// carry it.

// HelmClusterAddonRepository represents a Helm or OCI-compliant repository containing Helm charts that can be referenced by HelmClusterAddon resources.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:metadata:labels={heritage=deckhouse,module=operator-helm}
// +kubebuilder:resource:singular=helmclusteraddonrepository,scope=Cluster
// +kubebuilder:validation:XValidation:rule="self.metadata.name.size() <= 63",message="repository name must be at most 63 characters long"
// +kubebuilder:printcolumn:name="Status",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].status",description="The readiness status of the repository"
// +kubebuilder:printcolumn:name="Synced",type="string",JSONPath=".status.conditions[?(@.type=='Synced')].status",description="Repository synchronization status"
// +kubebuilder:printcolumn:name="Last Sync",type="date",JSONPath=".status.lastSuccessfulSyncTime",description="Time of the last successful catalog synchronization"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
// +kubebuilder:printcolumn:name="Next Sync",type="string",JSONPath=".status.nextSyncTime",priority=1,description="Scheduled time of the next synchronization attempt"
// +kubebuilder:printcolumn:name="Message",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].message",priority=1
// +genclient
// +genclient:nonNamespaced
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type HelmClusterAddonRepository struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   RepositorySpec   `json:"spec"`
	Status RepositoryStatus `json:"status,omitempty"`
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

// HelmClusterAddonRepositoryList contains a list of HelmClusterAddonRepositories.
// +kubebuilder:object:root=true
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type HelmClusterAddonRepositoryList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata"`

	// Items provides a list of HelmClusterAddonRepositories.
	Items []HelmClusterAddonRepository `json:"items"`
}
