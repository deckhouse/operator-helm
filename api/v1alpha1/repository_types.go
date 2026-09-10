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

// RepositorySpec, RepositoryAuth and RepositoryStatus are shared by every
// repository kind: HelmClusterAddonRepository, HelmApplicationRepository and
// HelmClusterApplicationRepository differ only in scope and in who may reference
// them. Declaring the shape once makes a divergence between the schemas impossible
// by construction, and lets the controller reconcile all three through one code
// path. The field descriptions are the ones the released HelmClusterAddonRepository
// CRD already carries: sharing them changes no generated schema.
//
// This note is outside every doc comment on purpose: a doc comment on a Spec or
// Status type becomes the description of the spec or status field in the CRD.

type RepositorySpec struct {
	// URL of the Helm repository. Supports http(s):// and oci:// protocols.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:XValidation:rule="self.matches('^(https?|oci)://.+$')",message="URL must have a valid protocol (http, https, oci) and a non-empty path"
	URL string `json:"url"`

	// Auth contains authentication credentials for the repository.
	// +optional
	Auth *RepositoryAuth `json:"auth,omitempty"`

	// CACertificate is the PEM encoded CA certificate for TLS verification.
	// +optional
	CACertificate string `json:"caCertificate,omitempty"`

	// InsecureSkipVerify disable TLS certificate verification.
	// +optional
	InsecureSkipVerify bool `json:"insecureSkipVerify,omitempty"`
}

type RepositoryAuth struct {
	// Repository authentication username.
	// +kubebuilder:validation:MinLength=1
	Username string `json:"username"`
	// Repository authentication password.
	// +kubebuilder:validation:MinLength=1
	Password string `json:"password"`
}

type RepositoryStatus struct {
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
	// means the repository will not recover without a change. While a synchronization is
	// running Reconciling carries the reason Synchronization, or ForceReconcile when the
	// pass was requested through the force reconcile annotation.
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
	// LastForceReconcileTime is the time the most recent force reconcile request was
	// processed. It records that the request was acted on, not that it succeeded:
	// the outcome is reported by Ready and Synced.
	// +optional
	LastForceReconcileTime *metav1.Time `json:"lastForceReconcileTime,omitempty"`
	// ConsecutiveFetchFailures counts consecutive failures to read from the repository.
	// It drives the retry backoff and resets on the first success.
	// +optional
	ConsecutiveFetchFailures int32 `json:"consecutiveFetchFailures,omitempty"`
}
