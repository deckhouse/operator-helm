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

	helmv2 "github.com/werf/3p-helm-controller/api/v2"
	sourcev1 "github.com/werf/nelm-source-controller/api/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
	ReasonHelmReleaseFailed = "HelmReleaseFailed"
)

type ReleaseService struct {
	BaseService

	TargetNamespace string
}

func NewReleaseService(client client.Client, scheme *runtime.Scheme, targetNamespace string) *ReleaseService {
	return &ReleaseService{
		BaseService: BaseService{
			Client: client,
			Scheme: scheme,
		},
		TargetNamespace: targetNamespace,
	}
}

type ReleaseResult struct {
	Status  ResourceStatus
	History helmv2.Snapshots
}

func (r ReleaseResult) GetStatus() ResourceStatus {
	return r.Status
}

func (r ReleaseResult) IsReady() bool {
	return r.Status.IsReady()
}

func (r ReleaseResult) GetConditionType() string {
	return ConditionTypeReady
}

func (s *ReleaseService) EnsureHelmRelease(ctx context.Context, addon *helmv1alpha1.HelmClusterAddon, repoType utils.InternalRepositoryType) ReleaseResult {
	logger := log.FromContext(ctx)

	existing := &helmv2.HelmRelease{
		ObjectMeta: metav1.ObjectMeta{
			Name:      utils.GetInternalHelmReleaseName(addon.Name),
			Namespace: addon.Spec.Namespace,
		},
	}

	op, err := controllerutil.CreateOrPatch(ctx, s.Client, existing, func() error {
		return applyHelmReleaseSpec(addon, existing, repoType)
	})
	if err != nil {
		return ReleaseResult{Status: Failed(
			addon,
			ReasonHelmReleaseFailed,
			"Failed to create helm release",
			fmt.Errorf("reconciling helm release: %w", err),
		)}
	}

	if cond, ok := utils.IsConditionObserved(existing.GetConditions(), ConditionTypeReady, existing.Generation); ok {
		logger.Info("Successfully reconciled helm release", "operation", op)
		return ReleaseResult{
			History: existing.Status.History,
			Status: ResourceStatus{
				Status:             cond.Status,
				ObservedGeneration: addon.Generation,
				Reason:             cond.Reason,
				Message:            cond.Message,
			},
		}
	}

	return ReleaseResult{Status: Unknown(addon, common.ReasonReconciling)}
}

func (s *ReleaseService) SuspendHelmRelease(ctx context.Context, addon *helmv1alpha1.HelmClusterAddon) ReleaseResult {
	helmRelease := &helmv2.HelmRelease{}
	if err := s.Client.Get(ctx, types.NamespacedName{
		Name:      utils.GetInternalHelmReleaseName(addon.Name),
		Namespace: s.TargetNamespace,
	}, helmRelease); err != nil {
		if apierrors.IsNotFound(err) {
			return ReleaseResult{Status: Success(addon)}
		}
		return ReleaseResult{Status: Failed(
			addon,
			ReasonHelmReleaseFailed,
			"Failed to suspend helm release",
			fmt.Errorf("getting helm release: %w", err),
		)}
	}

	if helmRelease.Spec.Suspend {
		return ReleaseResult{Status: Success(addon)}
	}

	base := helmRelease.DeepCopy()
	helmRelease.Spec.Suspend = true

	if err := s.Client.Patch(ctx, helmRelease, client.MergeFrom(base)); err != nil {
		return ReleaseResult{Status: Failed(
			addon,
			ReasonHelmReleaseFailed,
			"Failed to suspend helm release",
			fmt.Errorf("suspending helm release: %w", err),
		)}
	}
	return ReleaseResult{Status: Success(addon)}
}

func (s *ReleaseService) CleanupHelmRelease(ctx context.Context, addon *helmv1alpha1.HelmClusterAddon) error {
	nn := types.NamespacedName{Name: utils.GetInternalHelmReleaseName(addon.Name), Namespace: s.TargetNamespace}
	if err := s.ensureResourceDeleted(ctx, nn, &helmv2.HelmRelease{}); err != nil {
		return fmt.Errorf("failed to delete helm release: %w", err)
	}

	return nil
}

func applyHelmReleaseSpec(addon *helmv1alpha1.HelmClusterAddon, existing *helmv2.HelmRelease, repoType utils.InternalRepositoryType) error {
	if existing.Labels == nil {
		existing.Labels = map[string]string{}
	}

	existing.Labels[LabelManagedBy] = LabelManagedByValue
	existing.Labels[LabelSourceName] = addon.Name

	existing.Spec.ReleaseName = addon.Name
	existing.Spec.TargetNamespace = addon.Spec.Namespace
	existing.Spec.Values = addon.Spec.Values

	existing.Spec.Suspend = false

	switch repoType {
	case utils.InternalHelmRepository:
		existing.Spec.ChartRef = &helmv2.CrossNamespaceSourceReference{
			Kind:      sourcev1.HelmChartKind,
			Name:      utils.GetInternalHelmChartName(addon.Name),
			Namespace: addon.Spec.Namespace,
		}
	case utils.InternalOCIRepository:
		existing.Spec.ChartRef = &helmv2.CrossNamespaceSourceReference{
			Kind:      sourcev1.OCIRepositoryKind,
			Name:      addon.Spec.Chart.HelmClusterAddonRepository,
			Namespace: addon.Spec.Namespace,
		}
	default:
		return fmt.Errorf("invalid repository type: %s", repoType)
	}

	return nil
}
