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
	HelmApplicationRepositoryKind     = "HelmApplicationRepository"
	HelmApplicationRepositoryResource = "helmapplicationrepositories"

	// HelmApplicationRepositoryLabelSourceName stores the name of the source facade resource.
	HelmApplicationRepositoryLabelSourceName = "helm.deckhouse.io/application-repository"
)

// The "Next Sync" print column below is a string, not a date, on purpose: a date
// column prints how long ago its value was, and kubectl renders any instant more
// than a second in the future as <invalid>. nextSyncTime is always in the future.
//
// The name length is guarded by a CEL rule rather than by the schema because
// metadata.name has no schema of its own. The upper bound is not decorative: the
// repository name is stored as the value of the "repository" label on the objects
// of its chart catalog, and a label value cannot exceed 63 characters.
//
// Both notes are deliberately outside the doc comment below — controller-gen folds
// every non-marker line of that block into the resource's API description.

// HelmApplicationRepository represents a Helm or OCI-compliant repository containing Helm charts that can be referenced by HelmApplication resources from the same namespace.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:metadata:labels={heritage=deckhouse,module=operator-helm}
// +kubebuilder:resource:singular=helmapplicationrepository,scope=Namespaced
// +kubebuilder:validation:XValidation:rule="self.metadata.name.size() >= 3 && self.metadata.name.size() <= 63",message="repository name must be between 3 and 63 characters long"
// +kubebuilder:printcolumn:name="Status",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].status",description="The readiness status of the repository"
// +kubebuilder:printcolumn:name="Synced",type="string",JSONPath=".status.conditions[?(@.type=='Synced')].status",description="Repository synchronization status"
// +kubebuilder:printcolumn:name="Last Sync",type="date",JSONPath=".status.lastSuccessfulSyncTime",description="Time of the last successful catalog synchronization"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
// +kubebuilder:printcolumn:name="Next Sync",type="string",JSONPath=".status.nextSyncTime",priority=1,description="Scheduled time of the next synchronization attempt"
// +kubebuilder:printcolumn:name="Message",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].message",priority=1
// +genclient
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type HelmApplicationRepository struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   RepositorySpec   `json:"spec"`
	Status RepositoryStatus `json:"status,omitempty"`
}

func (r *HelmApplicationRepository) GetConditions() *[]metav1.Condition {
	return &r.Status.Conditions
}

func (r *HelmApplicationRepository) SetObservedGeneration(generation int64) {
	r.Status.ObservedGeneration = generation
}

func (r *HelmApplicationRepository) GetObservedGeneration() int64 {
	return r.Status.ObservedGeneration
}

func (r *HelmApplicationRepository) GetStatus() any {
	return r.Status
}

func (r *HelmApplicationRepository) GetConditionTypesForUpdate() []string {
	return []string{ConditionTypeReady}
}

func (r *HelmApplicationRepository) ForceReconcileRequired() bool {
	annotations := r.GetAnnotations()
	if annotations == nil {
		return false
	}

	_, found := annotations[AnnotationForceReconcile]

	return found
}

// HelmApplicationRepositoryList contains a list of HelmApplicationRepositories.
// +kubebuilder:object:root=true
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type HelmApplicationRepositoryList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata"`

	// Items provides a list of HelmApplicationRepositories.
	Items []HelmApplicationRepository `json:"items"`
}
