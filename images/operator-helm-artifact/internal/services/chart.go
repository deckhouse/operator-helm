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

package services

import (
	"context"
	"fmt"

	"github.com/werf/3p-fluxcd-pkg/apis/meta"
	sourcev1 "github.com/werf/nelm-source-controller/api/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
	"github.com/deckhouse/operator-helm/internal/common"
	"github.com/deckhouse/operator-helm/internal/utils"
)

const (
	ConditionTypeReleaseChart = "ConditionTypeReleaseChart"
	ReasonHelmChartFailed     = "HelmChartFailed"
)

type ChartService struct {
	BaseService

	TargetNamespace string
}

func NewChartService(client client.Client, scheme *runtime.Scheme, targetNamespace string) *ChartService {
	return &ChartService{
		BaseService: BaseService{
			Client: client,
			Scheme: scheme,
		},
		TargetNamespace: targetNamespace,
	}
}

type ChartResult struct {
	Status   ResourceStatus
	Artifact *meta.Artifact
}

func (r ChartResult) GetStatus() ResourceStatus {
	return r.Status
}

func (r ChartResult) IsReady() bool {
	return r.Artifact != nil
}

func (r ChartResult) IsPartiallyDegraded() bool {
	return r.Status.IsReady()
}

func (r ChartResult) GetConditionType() string {
	return ConditionTypeReleaseChart
}

func (s *ChartService) EnsureHelmChart(ctx context.Context, addon *helmv1alpha1.HelmClusterAddon, repo *helmv1alpha1.HelmClusterAddonRepository) ChartResult {
	logger := log.FromContext(ctx)

	existing := &sourcev1.HelmChart{
		ObjectMeta: metav1.ObjectMeta{
			Name:      utils.GetInternalHelmChartName(addon.Name),
			Namespace: s.TargetNamespace,
		},
	}

	op, err := controllerutil.CreateOrPatch(ctx, s.Client, existing, func() error {
		applyHelmChartSpec(addon, existing)

		return nil
	})
	if err != nil {
		return ChartResult{Status: Failed(
			addon,
			ReasonHelmChartFailed,
			"Failed to create helm chart",
			fmt.Errorf("creating or updating helm chart: %w", err),
		)}
	}

	if op != controllerutil.OperationResultNone {
		logger.Info("Reconciled helm chart", "operation", op)
	}

	if cond, ok := utils.IsConditionObserved(existing.GetConditions(), ConditionTypeReleaseChart, existing.Generation); ok {
		logger.Info("Successfully reconciled helm chart", "operation", op, "chart", addon.Spec.Chart.HelmClusterAddonChartName)
		return ChartResult{
			Artifact: existing.Status.Artifact,
			Status: ResourceStatus{
				Status:             cond.Status,
				ObservedGeneration: addon.Generation,
				Reason:             cond.Reason,
				Message:            cond.Message,
			},
		}
	}

	return ChartResult{Status: Unknown(addon, common.ReasonReconciling)}
}

func (s *ChartService) CleanupHelmChart(ctx context.Context, addon *helmv1alpha1.HelmClusterAddon) error {
	nn := types.NamespacedName{Name: utils.GetInternalHelmChartName(addon.Name), Namespace: s.TargetNamespace}
	if err := s.ensureResourceDeleted(ctx, nn, &sourcev1.HelmChart{}); err != nil {
		return fmt.Errorf("failed to delete helm chart: %w", err)
	}

	return nil
}

func applyHelmChartSpec(addon *helmv1alpha1.HelmClusterAddon, existing *sourcev1.HelmChart) {
	if existing.Labels == nil {
		existing.Labels = map[string]string{}
	}

	existing.Labels[LabelManagedBy] = LabelManagedByValue
	existing.Labels[LabelSourceName] = addon.Name
	existing.Labels[helmv1alpha1.HelmClusterAddonLabelSourceName] = utils.GetHelmClusterAddonChartName(
		addon.Spec.Chart.HelmClusterAddonRepository, addon.Spec.Chart.HelmClusterAddonChartName)

	existing.Spec.Chart = addon.Spec.Chart.HelmClusterAddonChartName
	existing.Spec.Version = addon.Spec.Chart.Version

	existing.Spec.SourceRef = sourcev1.LocalHelmChartSourceReference{
		Kind: sourcev1.HelmRepositoryKind,
		Name: addon.Spec.Chart.HelmClusterAddonRepository,
	}
}
