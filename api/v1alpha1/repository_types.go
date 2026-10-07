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
// path.
//
// This note is outside every doc comment on purpose: a doc comment on a Spec or
// Status type becomes the description of the spec or status field in the CRD.

type RepositorySpec struct {
	// URL of the Helm or OCI repository.
	//
	// Supported schemes: `http(s)://` and `oci://`.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:XValidation:rule="self.matches('^(https?|oci)://.+$')",message="URL must have a valid protocol (http, https, oci) and a non-empty path"
	URL string `json:"url"`

	// Credentials for repository authentication.
	// +optional
	Auth *RepositoryAuth `json:"auth,omitempty"`

	// CA certificate in PEM format for verifying the repository TLS certificate.
	// +optional
	CACertificate string `json:"caCertificate,omitempty"`

	// Disables verification of the repository TLS certificate.
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
	// Conditions reflecting the current state of the repository.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// Latest resource generation processed by the controller.
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// Time of the last successful repository synchronization.
	// +optional
	LastSuccessfulSyncTime *metav1.Time `json:"lastSuccessfulSyncTime,omitempty"`
	// Scheduled time of the next repository synchronization attempt.
	// +optional
	NextSyncTime *metav1.Time `json:"nextSyncTime,omitempty"`
	// Time when the last forced reconciliation request was processed.
	//
	// This value indicates that the request was processed but does not indicate that reconciliation completed successfully.
	// Reconciliation results are reflected in the `Ready` and `Synced` conditions.
	// +optional
	LastForceReconcileTime *metav1.Time `json:"lastForceReconcileTime,omitempty"`
	// Number of consecutive failed attempts to access the repository.
	//
	// Used to determine the delay before the next attempt and reset after a successful attempt.
	// +optional
	ConsecutiveFetchFailures int32 `json:"consecutiveFetchFailures,omitempty"`
	// Number of charts discovered during the last successful repository synchronization.
	//
	// The field is not populated until the first successful synchronization, which distinguishes a repository that has not yet been synchronized from a repository that contains no charts.
	// +optional
	ChartCount *int32 `json:"chartCount,omitempty"`
}
