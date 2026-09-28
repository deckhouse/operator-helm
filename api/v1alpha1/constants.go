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

	// LabelSourceNamespace carries the namespace of the namespaced source resource
	// an internal object was derived from.
	LabelSourceNamespace = "helm.deckhouse.io/source-namespace"

	LabelDeckhouseHeritage      = "heritage"
	LabelDeckhouseHeritageValue = "deckhouse"

	AnnotationForceReconcile = "reconcile.helm.deckhouse.io/force"

	// LabelRepositoryName and LabelChartName are set on every chart catalog object
	// and carry the repository/chart pair the object mirrors.
	LabelRepositoryName = "repository"
	LabelChartName      = "chart"

	// UnavailableReason* are the values of the UnavailableReason field of a chart
	// catalog version, in every family.

	// UnavailableReasonRemovedFromRepository means the tag is no longer offered by
	// the repository.
	UnavailableReasonRemovedFromRepository = "RemovedFromRepository"
	// UnavailableReasonUnsupportedMediaType means the manifest was read but the
	// artifact is not a packaged Helm chart.
	UnavailableReasonUnsupportedMediaType = "UnsupportedMediaType"
	// UnavailableReasonResolvePending means the manifest request failed and no
	// verdict was reached.
	UnavailableReasonResolvePending = "ResolvePending"
	// UnavailableReasonInvalidChartReference means the repository index points this
	// version at a registry with a reference that is not a valid tagged reference.
	UnavailableReasonInvalidChartReference = "InvalidChartReference"
)
