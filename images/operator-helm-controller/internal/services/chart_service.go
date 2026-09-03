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

	"github.com/deckhouse/operator-helm/api/naming"
	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
	"github.com/deckhouse/operator-helm/internal/manager/status"
	"github.com/deckhouse/operator-helm/internal/utils"
)

var helmChartErrorRules = []status.ErrorConditionRule{
	{Type: "FetchFailed", TriggerStatus: metav1.ConditionTrue, Reason: helmv1alpha1.ReasonChartFetchFailed},
	{Type: "StorageOperationFailed", TriggerStatus: metav1.ConditionTrue, Reason: helmv1alpha1.ReasonChartStorageFailed},
}

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

var _ status.Provider = (*ChartResult)(nil)

type ChartResult struct {
	Status   status.Status
	Artifact *meta.Artifact
}

func (r ChartResult) GetStatus() status.Status {
	return r.Status
}

func (r ChartResult) IsReady() bool {
	return r.Artifact != nil && r.Status.Observed && r.Status.Status == metav1.ConditionTrue
}

func (r ChartResult) HasArtifact() bool {
	return r.Artifact != nil && r.Status.Observed
}

func (r ChartResult) GetConditionType() string {
	return helmv1alpha1.ConditionTypeReady
}

func (s *ChartService) EnsureHelmChart(ctx context.Context, addon *helmv1alpha1.HelmClusterAddon) ChartResult {
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
		return ChartResult{Status: status.Failed(
			addon,
			helmv1alpha1.ReasonHelmChartFailed,
			"Failed to create helm chart",
			fmt.Errorf("creating or updating helm chart: %w", err),
		)}
	}

	if op != controllerutil.OperationResultNone {
		logger.Info("Reconciled helm chart", "operation", op)
	}

	processedStatus := status.ProcessChildConditions(
		existing.GetConditions(), existing.Generation, addon, helmChartErrorRules,
	)

	if processedStatus.IsReady() {
		logger.Info("Successfully reconciled helm chart", "operation", op, "chart", addon.Spec.Chart.HelmClusterAddonChartName)
	}

	return ChartResult{
		Artifact: existing.Status.Artifact,
		Status:   processedStatus,
	}
}

// CleanupHelmChart issues a delete for the internal HelmChart and returns it
// while it is still present, so the caller can inspect its conditions and wait
// for nelm-source-controller to finish removing it. It returns nil once the
// HelmChart is gone.
func (s *ChartService) CleanupHelmChart(ctx context.Context, addon *helmv1alpha1.HelmClusterAddon) (*sourcev1.HelmChart, error) {
	nn := types.NamespacedName{Name: utils.GetInternalHelmChartName(addon.Name), Namespace: s.TargetNamespace}
	chart := &sourcev1.HelmChart{}
	exists, err := s.deleteAndCheck(ctx, nn, chart)
	if err != nil {
		return nil, fmt.Errorf("failed to delete helm chart: %w", err)
	}
	if !exists {
		return nil, nil
	}

	return chart, nil
}

func applyHelmChartSpec(addon *helmv1alpha1.HelmClusterAddon, existing *sourcev1.HelmChart) {
	if addon.ForceReconcileRequired() {
		setReconcileRequestAnnotations(existing)
	}

	if existing.Labels == nil {
		existing.Labels = map[string]string{}
	}

	existing.Labels[helmv1alpha1.LabelManagedBy] = helmv1alpha1.LabelManagedByValue
	existing.Labels[helmv1alpha1.HelmClusterAddonLabelSourceName] = addon.Name
	existing.Labels[helmv1alpha1.HelmClusterAddonChartLabelSourceName] = naming.HelmClusterAddonChartName(
		addon.Spec.Chart.HelmClusterAddonRepository, addon.Spec.Chart.HelmClusterAddonChartName,
	)

	existing.Spec.Chart = addon.Spec.Chart.HelmClusterAddonChartName
	existing.Spec.Version = addon.Spec.Chart.Version

	existing.Spec.SourceRef = sourcev1.LocalHelmChartSourceReference{
		Kind: sourcev1.HelmRepositoryKind,
		Name: utils.GetInternalHelmRepositoryName(addon.Spec.Chart.HelmClusterAddonRepository),
	}
}
