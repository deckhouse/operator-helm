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

const (
	// TargetNamespace is the namespace where internal customer resources are created.
	TargetNamespace = "d8-operator-helm"

	// FinalizerName is the finalizer used to ensure cleanup.
	FinalizerName = "helm.deckhouse.io/cleanup"

	// LabelManagedBy marks resources as managed by this controller.
	LabelManagedBy = "helm.deckhouse.io/managed-by"

	// LabelManagedByValue is the value for the managed-by label.
	LabelManagedByValue = "operator-helm"

	LabelDeckhouseHeritage      = "heritage"
	LabelDeckhouseHeritageValue = "deckhouse"

	AnnotationForceReconcile = "reconcile.helm.deckhouse.io/force"

	// LabelRepositoryName and LabelChartName are set on every chart catalog object —
	// HelmClusterAddonChart, HelmApplicationChart and HelmClusterApplicationChart —
	// and carry the repository/chart pair the object mirrors. They are the only way
	// back from the object name — a truncated hash — to the pair it belongs to, which
	// is why both the catalog synchronization and the watch that maps a chart to the
	// resources using it read them.
	LabelRepositoryName = "repository"
	LabelChartName      = "chart"

	// UnavailableReason* are the values of the UnavailableReason field of a chart
	// catalog version, in every family. They are field values rather than condition
	// reasons, and they describe OCI artifacts rather than resource kinds, so they
	// live here instead of conditions.go or next to one family's chart type.
	//
	// UnavailableReasonRemovedFromRepository means the tag is no longer offered by the
	// repository. The entry is retained only because a resource still references it, and
	// the marker is dropped automatically once the tag is listed again.
	UnavailableReasonRemovedFromRepository = "RemovedFromRepository"
	// UnavailableReasonUnsupportedMediaType means the manifest was read but the artifact
	// is not a packaged Helm chart. It is a verdict about the artifact, so it is kept
	// until a force reconcile re-examines every tag.
	UnavailableReasonUnsupportedMediaType = "UnsupportedMediaType"
	// UnavailableReasonResolvePending means the manifest request failed and no verdict
	// was reached. Such a tag is re-examined on every normal synchronization.
	UnavailableReasonResolvePending = "ResolvePending"
	// UnavailableReasonInvalidChartReference means the repository index points this
	// version at a registry, but the reference it gives is not a valid tagged
	// reference. It is a verdict about the index entry rather than about the
	// artifact, so it is kept until the repository publishes a usable reference.
	UnavailableReasonInvalidChartReference = "InvalidChartReference"
)
