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
	"context"
	"fmt"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

const (
	HelmClusterAddonKind     = "HelmClusterAddon"
	HelmClusterAddonResource = "helmclusteraddons"
)

// HelmClusterAddon represents a Helm addon that is installed across the whole cluster.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:metadata:labels={heritage=deckhouse,module=operator-helm}
// +kubebuilder:resource:categories={all,operator-helm},singular=helmclusteraddon,scope=Cluster
// +kubebuilder:printcolumn:name="Chart Name",type="string",JSONPath=".spec.chart.helmClusterAddonChart",description="Helm release chart name."
// +kubebuilder:printcolumn:name="Chart Version",type="string",JSONPath=".spec.chart.version",description="Helm release chart version."
// +kubebuilder:printcolumn:name="Status",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].status",description="The readiness status of the repository"
// +genclient
// +genclient:nonNamespaced
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type HelmClusterAddon struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   HelmClusterAddonSpec   `json:"spec"`
	Status HelmClusterAddonStatus `json:"status,omitempty"`
}

func (r *HelmClusterAddon) SetupWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr, r).
		WithValidator(&HelmClusterAddonValidator{Client: mgr.GetClient()}).
		Complete()
}

type HelmClusterAddonSpec struct {
	Chart HelmClusterAddonChartRef `json:"chart"`
	// Values holds the values for this HelmClusterAddon release.
	// +kubebuilder:pruning:PreserveUnknownFields
	// +optional
	Values *apiextensionsv1.JSON `json:"values"`
	// Namespace to deploy cluster addon release
	// +kubebuilder:default:="default"
	// +optional
	// +kubebuilder:validation:MinLength=3
	// +kubebuilder:validation:MaxLength=63
	Namespace string `json:"namespace"`
	// Maintenance specifies the reconciliation strategy for the resource.
	// When set to "NoResourceReconciliation", the controller will stop updating the
	// underlying resources, allowing for manual intervention or maintenance
	// without the operator overwriting changes.
	// When empty (""), standard reconciliation is active.
	// +kubebuilder:validation:Enum="";NoResourceReconciliation
	// +optional
	Maintanace string `json:"maintanace,omitempty"`
}

type HelmClusterAddonChartRef struct {
	// Specifies the name of the Helm chart to be installed
	// from the defined repository (e.g., "ingress-nginx" or "redis").
	// +kubebuilder:validation:MinLength=1
	HelmClusterAddonChartName string `json:"helmClusterAddonChart"`
	// Specifies the name of the HelmClusterAddonRepository custom resource that contains
	// the connection details and credentials for the repository where
	// the chart is located.
	// +kubebuilder:validation:MinLength=3
	// +kubebuilder:validation:MaxLength=63
	HelmClusterAddonRepository string `json:"helmClusterAddonRepository"`
	// Versions holds the HelmClusterAddon chart version.
	// +optional
	Version string `json:"version,omitempty"`
}

type HelmClusterAddonStatus struct {
	// Conditions represent the latest available observations of the repository state.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// Generating a resource that was last processed by the controller.
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// HelmClusterAddonList contains a list of HelmClusterAddons.
// +kubebuilder:object:root=true
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type HelmClusterAddonList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata"`

	// Items provides a list of HelmClusterAddons.
	Items []HelmClusterAddon `json:"items"`
}

// HelmClusterAddonMaintanace describe HelmClusterAddon maintanance operation mode.
// +kubebuilder:validation:Enum={"",NoResourceReconciliation}
type HelmClusterAddonMaintanace string

const (
	NoResourceReconciliation HelmClusterAddonMaintanace = "NoResourceReconciliation"
)

// +k8s:deepcopy-gen=false
type HelmClusterAddonValidator struct {
	Client client.Client
}

func (v *HelmClusterAddonValidator) ValidateCreate(ctx context.Context, addon *HelmClusterAddon) (admission.Warnings, error) {
	return nil, v.checkUniqueness(ctx, addon)
}

func (v *HelmClusterAddonValidator) ValidateUpdate(ctx context.Context, _, newObj *HelmClusterAddon) (admission.Warnings, error) {
	return nil, v.checkUniqueness(ctx, newObj)
}

func (v *HelmClusterAddonValidator) ValidateDelete(_ context.Context, _ *HelmClusterAddon) (admission.Warnings, error) {
	return nil, nil
}

func (v *HelmClusterAddonValidator) checkUniqueness(ctx context.Context, addon *HelmClusterAddon) error {
	list := &HelmClusterAddonList{}

	if err := v.Client.List(ctx, list); err != nil {
		return err
	}

	for _, existing := range list.Items {
		if existing.Name != addon.Name &&
			existing.Spec.Chart.HelmClusterAddonRepository == addon.Spec.Chart.HelmClusterAddonRepository &&
			existing.Spec.Chart.HelmClusterAddonChartName == addon.Spec.Chart.HelmClusterAddonChartName {
			return fmt.Errorf(
				"chart %s is already used by helmclusteraddon/%s",
				addon.Spec.Chart.HelmClusterAddonChartName, existing.Name,
			)
		}
	}

	return nil
}
