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

package helmclusteraddonrepository

import (
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
	sourcev1 "github.com/werf/nelm-source-controller/api/v1"
)

// BuildDesiredHelmRepository constructs the desired state of the internal
// HelmRepository from the given HelmClusterAddonRepository. The returned object
// is not persisted — the caller is responsible for creating or patching.
func BuildDesiredHelmRepository(src *helmv1alpha1.HelmClusterAddonRepository) *sourcev1.HelmRepository {
	repoType := sourcev1.HelmRepositoryTypeDefault

	// TODO: need to involve a factory to handle different schemas
	if strings.HasPrefix(src.Spec.URL, "oci://") {
		repoType = sourcev1.HelmRepositoryTypeOCI
	}

	dst := &sourcev1.HelmRepository{
		ObjectMeta: metav1.ObjectMeta{
			Name:      src.Name,
			Namespace: TargetNamespace,
			Labels: map[string]string{
				LabelManagedBy:  LabelManagedByValue,
				LabelSourceName: src.Name,
			},
		},
		Spec: sourcev1.HelmRepositorySpec{
			URL:  src.Spec.URL,
			Type: repoType,
			Interval: metav1.Duration{
				// TODO: remove magic number
				Duration: 10 * time.Minute,
			},
		},
	}

	// Map TLSVerify → Insecure (inverted semantics).
	// Insecure is only meaningful for OCI repositories, but we set it
	// consistently so the intent is clear.
	if !src.Spec.TLSVerify {
		dst.Spec.Insecure = true
	}

	return dst
}

// ApplyDesiredSpec updates an existing HelmRepository's spec fields to match
// the desired state. Returns true if any field was changed.
func ApplyDesiredSpec(existing *sourcev1.HelmRepository, desired *sourcev1.HelmRepository) bool {
	changed := false

	if existing.Spec.URL != desired.Spec.URL {
		existing.Spec.URL = desired.Spec.URL
		changed = true
	}
	if existing.Spec.Type != desired.Spec.Type {
		existing.Spec.Type = desired.Spec.Type
		changed = true
	}
	if existing.Spec.Insecure != desired.Spec.Insecure {
		existing.Spec.Insecure = desired.Spec.Insecure
		changed = true
	}
	if existing.Spec.Interval != desired.Spec.Interval {
		existing.Spec.Interval = desired.Spec.Interval
		changed = true
	}

	// Ensure labels are up to date.
	if existing.Labels == nil {
		existing.Labels = make(map[string]string)
	}
	for k, v := range desired.Labels {
		if existing.Labels[k] != v {
			existing.Labels[k] = v
			changed = true
		}
	}

	return changed
}

// TODO: need to re-work these statuses according to adr

// MapInternalStatusToClusterConditions translates the internal HelmRepository
// status into conditions suitable for the HelmClusterRepository status.
func MapInternalStatusToClusterConditions(internal *sourcev1.HelmRepository) []metav1.Condition {
	now := metav1.Now()

	// Find the Ready condition on the internal resource.
	var readyCond *metav1.Condition
	for i := range internal.Status.Conditions {
		if internal.Status.Conditions[i].Type == ConditionTypeReady {
			readyCond = &internal.Status.Conditions[i]
			break
		}
	}

	if readyCond == nil {
		return []metav1.Condition{
			{
				Type:               ConditionTypeReady,
				Status:             metav1.ConditionUnknown,
				Reason:             ReasonInternalNotReady,
				Message:            "Initializing repository..",
				LastTransitionTime: now,
			},
		}
	}

	reason := ReasonInternalNotReady
	if readyCond.Status == metav1.ConditionTrue {
		reason = ReasonInternalReady
	}

	return []metav1.Condition{
		{
			Type:               ConditionTypeReady,
			Status:             readyCond.Status,
			Reason:             reason,
			Message:            readyCond.Message,
			LastTransitionTime: now,
		},
	}
}
