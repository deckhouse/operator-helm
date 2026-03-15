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

package helmclusteraddon

import (
	"context"
	"fmt"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
)

func SetupWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr, &helmv1alpha1.HelmClusterAddon{}).
		WithValidator(&UniqRepositoryAndChartNameWebhookValidator{Client: mgr.GetClient()}).
		Complete()
}

type UniqRepositoryAndChartNameWebhookValidator struct {
	Client client.Client
}

func (v *UniqRepositoryAndChartNameWebhookValidator) ValidateCreate(ctx context.Context, addon *helmv1alpha1.HelmClusterAddon) (admission.Warnings, error) {
	return nil, v.checkUniqueness(ctx, addon)
}

func (v *UniqRepositoryAndChartNameWebhookValidator) ValidateUpdate(ctx context.Context, _, newObj *helmv1alpha1.HelmClusterAddon) (admission.Warnings, error) {
	return nil, v.checkUniqueness(ctx, newObj)
}

func (v *UniqRepositoryAndChartNameWebhookValidator) ValidateDelete(_ context.Context, _ *helmv1alpha1.HelmClusterAddon) (admission.Warnings, error) {
	return nil, nil
}

func (v *UniqRepositoryAndChartNameWebhookValidator) checkUniqueness(ctx context.Context, addon *helmv1alpha1.HelmClusterAddon) error {
	list := &helmv1alpha1.HelmClusterAddonList{}

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
