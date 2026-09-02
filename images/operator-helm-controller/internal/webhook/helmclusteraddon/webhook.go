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
	"slices"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
	"github.com/deckhouse/operator-helm/internal/index"
	"github.com/deckhouse/operator-helm/internal/services"
	"github.com/deckhouse/operator-helm/internal/utils"
)

var uniquenessBypassUsernames = []string{
	"system:serviceaccount:d8-operator-helm:operator-helm-controller",
}

func SetupWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr, &helmv1alpha1.HelmClusterAddon{}).
		WithValidator(&HelmClusterAddonWebhookValidator{
			Client:       mgr.GetClient(),
			claimService: services.NewClaimService(mgr.GetClient(), mgr.GetAPIReader(), helmv1alpha1.TargetNamespace),
		}).
		Complete()
}

var _ admission.Validator[*helmv1alpha1.HelmClusterAddon] = (*HelmClusterAddonWebhookValidator)(nil)

type HelmClusterAddonWebhookValidator struct {
	Client       client.Client
	claimService *services.ClaimService
}

func (v *HelmClusterAddonWebhookValidator) ValidateCreate(ctx context.Context, addon *helmv1alpha1.HelmClusterAddon) (admission.Warnings, error) {
	if err := validateNotSystemNamespace(addon); err != nil {
		return nil, err
	}

	if isUniquenessBypassed(ctx) {
		return nil, nil
	}

	return nil, v.checkUniqueness(ctx, addon)
}

func (v *HelmClusterAddonWebhookValidator) ValidateUpdate(ctx context.Context, oldObj, newObj *helmv1alpha1.HelmClusterAddon) (admission.Warnings, error) {
	if err := validateNotSystemNamespace(newObj); err != nil {
		return nil, err
	}

	if isUniquenessBypassed(ctx) {
		return nil, nil
	}

	if forceReconcileToggled(oldObj, newObj) && !chartRefChanged(oldObj, newObj) {
		return nil, nil
	}

	return nil, v.checkUniqueness(ctx, newObj)
}

func (v *HelmClusterAddonWebhookValidator) ValidateDelete(_ context.Context, addon *helmv1alpha1.HelmClusterAddon) (admission.Warnings, error) {
	if addon.Spec.Maintenance == "NoResourceReconciliation" {
		return nil, fmt.Errorf("helmclusteraddon/%s cannot be deleted while maintenance mode is active", addon.Name)
	}

	return nil, nil
}

func validateNotSystemNamespace(addon *helmv1alpha1.HelmClusterAddon) error {
	if utils.IsSystemNamespace(addon.Spec.Namespace) {
		return fmt.Errorf("helmclusteraddon/%s cannot use system namespace %s as target namespace", addon.Name, addon.Spec.Namespace)
	}

	return nil
}

func forceReconcileToggled(oldObj, newObj *helmv1alpha1.HelmClusterAddon) bool {
	return oldObj.Annotations[helmv1alpha1.AnnotationForceReconcile] != newObj.Annotations[helmv1alpha1.AnnotationForceReconcile]
}

func chartRefChanged(oldObj, newObj *helmv1alpha1.HelmClusterAddon) bool {
	return oldObj.Spec.Chart.HelmClusterAddonRepository != newObj.Spec.Chart.HelmClusterAddonRepository ||
		oldObj.Spec.Chart.HelmClusterAddonChartName != newObj.Spec.Chart.HelmClusterAddonChartName
}

func isUniquenessBypassed(ctx context.Context) bool {
	req, err := admission.RequestFromContext(ctx)
	if err != nil {
		return false
	}

	return slices.Contains(uniquenessBypassUsernames, req.UserInfo.Username)
}

func (v *HelmClusterAddonWebhookValidator) checkUniqueness(ctx context.Context, addon *helmv1alpha1.HelmClusterAddon) error {
	owned, err := v.claimService.OwnedBy(ctx, addon)
	if err != nil {
		return fmt.Errorf("failed to check if helmclusteraddon/%s owns chart claim: %w", addon.Name, err)
	}
	if owned {
		return nil
	}

	list := &helmv1alpha1.HelmClusterAddonList{}
	indexValue := index.AddonChartValue(addon.Spec.Chart.HelmClusterAddonRepository, addon.Spec.Chart.HelmClusterAddonChartName)

	if err := v.Client.List(ctx, list, client.MatchingFields{index.AddonChart: indexValue}); err != nil {
		return err
	}

	for _, existing := range list.Items {
		if existing.Name != addon.Name {
			return fmt.Errorf(
				"chart %s is already used by helmclusteraddon/%s",
				addon.Spec.Chart.HelmClusterAddonChartName, existing.Name,
			)
		}
	}

	return nil
}
