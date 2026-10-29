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

// This note is outside every doc comment on purpose: a doc comment on a Spec or Status type becomes the description of the spec or status field in the CRD.

type RepositorySpec struct {
	// URL of the Helm or OCI repository.
	//
	// Supported schemes: `http(s)://` and `oci://`. An `oci://` URL points to the repository of a single Helm chart, for example `oci://registry.example.com/charts/podinfo`.
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
	//
	// Applies to an OCI registry only. For a Helm repository served over HTTPS with a self-signed certificate, set `caCertificate`.
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
	// +crd-enricher:raw:items.properties.lastTransitionTime.description=Time when the condition status last changed.
	// +crd-enricher:raw:items.properties.message.description=Message with additional information about the condition state.
	// +crd-enricher:raw:items.properties.observedGeneration.description=Resource generation on which the condition state is based.
	// +crd-enricher:raw:items.properties.reason.description=Reason for the last change in the condition state.
	// +crd-enricher:raw:items.properties.status.description=Condition status.
	// +crd-enricher:raw:items.properties.type.description=Condition type.
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// Latest resource generation processed by the controller.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// Time of the last successful repository synchronization.
	// +optional
	LastSuccessfulSyncTime *metav1.Time `json:"lastSuccessfulSyncTime,omitempty"`
	// Resource generation that the currently published chart catalog corresponds to.
	//
	// Updated when a synchronization publishes the catalog without running out of its time budget. Retained if later synchronizations fail or run out of time.
	// +optional
	LastSyncedGeneration int64 `json:"lastSyncedGeneration,omitempty"`
	// Number of Helm chart versions offered by the repository that the last synchronization publishing the catalog did not examine because it ran out of time. Retained if later synchronizations fail.
	//
	// Such versions are examined during the next synchronizations. A value of `0` means that every version offered by the repository has been examined.
	// +optional
	UnexaminedVersions int32 `json:"unexaminedVersions,omitempty"`
	// Scheduled time of the next repository synchronization attempt.
	// +optional
	NextSyncTime *metav1.Time `json:"nextSyncTime,omitempty"`
	// Time when the last forced reconciliation request was processed.
	//
	// This value indicates that the request was processed but does not indicate that reconciliation completed successfully.
	// Reconciliation results are reflected in the `Ready` and `Synced` conditions.
	// +optional
	LastForceReconcileTime *metav1.Time `json:"lastForceReconcileTime,omitempty"`
	// Counter of consecutive failed attempts to access the repository, used to determine the delay before the next attempt.
	//
	// Does not exceed `5`: an error that retries cannot fix sets it to `5` at once. Reset after a successful attempt and when the repository specification changes.
	// +optional
	ConsecutiveFetchFailures int32 `json:"consecutiveFetchFailures,omitempty"`
	// Number of charts found during the last successful read of the repository, excluding the charts whose name cannot be recorded on a catalog object.
	//
	// The field is not populated until the first successful read, which distinguishes a repository that has not been read yet from a repository that contains no charts.
	// +optional
	ChartCount *int32 `json:"chartCount,omitempty"`
}
