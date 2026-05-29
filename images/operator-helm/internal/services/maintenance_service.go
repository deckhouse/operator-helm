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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
	statusmgr "github.com/deckhouse/operator-helm/internal/manager/status"
	"github.com/deckhouse/operator-helm/internal/utils"
)

type MaintenanceService struct {
	BaseService

	TargetNamespace string
}

func NewMaintenanceService(client client.Client, scheme *runtime.Scheme, targetNamespace string) *MaintenanceService {
	return &MaintenanceService{
		BaseService: BaseService{
			Client: client,
			Scheme: scheme,
		},
		TargetNamespace: targetNamespace,
	}
}

var _ statusmgr.Provider = (*MaintenanceResult)(nil)

type MaintenanceResult struct {
	Status statusmgr.Status
}

func (r MaintenanceResult) GetStatus() statusmgr.Status {
	return r.Status
}

func (r MaintenanceResult) IsReady() bool {
	return r.Status.IsReady()
}

func (r MaintenanceResult) GetConditionType() string {
	return helmv1alpha1.ConditionTypeManaged
}

func (s *MaintenanceService) EnsureMaintenanceMode(ctx context.Context, addon *helmv1alpha1.HelmClusterAddon) MaintenanceResult {
	logger := log.FromContext(ctx)

	suspendState := addon.MaintenanceModeActivated()
	status := metav1.ConditionTrue
	reason := helmv1alpha1.ReasonMaintenanceModeInactive

	var message string

	if suspendState {
		logger.Info("Enabling maintenance mode")
		message = "Maintenance mode enabled"
		status = metav1.ConditionFalse
		reason = helmv1alpha1.ReasonMaintenanceModeActive
	} else {
		logger.Info("Disabling maintenance mode")
		message = "Maintenance mode disabled"
	}

	err := s.updateHelmReleaseSuspendState(ctx, addon, suspendState)
	if err != nil {
		return MaintenanceResult{Status: statusmgr.Failed(addon, helmv1alpha1.ReasonFailed, "Failed to change maintenance mode", err)}
	}
	return MaintenanceResult{
		Status: statusmgr.Status{
			Observed:           true,
			Status:             status,
			ObservedGeneration: addon.Generation,
			Message:            message,
			Reason:             reason,
		},
	}
}

func (s *MaintenanceService) IsMaintenanceModeChangeRequired(addon *helmv1alpha1.HelmClusterAddon) bool {
	if addon.MaintenanceModeActivated() && !addon.MaintenanceModeEnabled() {
		return true
	}

	if !addon.MaintenanceModeActivated() && (addon.MaintenanceModeEnabled() ||
		apimeta.IsStatusConditionPresentAndEqual(addon.Status.Conditions, helmv1alpha1.ConditionTypeManaged, metav1.ConditionUnknown)) {
		return true
	}

	return false
}

func (s *MaintenanceService) updateHelmReleaseSuspendState(ctx context.Context, addon *helmv1alpha1.HelmClusterAddon, suspend bool) error {
	helmRelease := &helmv2.HelmRelease{}
	if err := s.Client.Get(ctx, types.NamespacedName{
		Name:      utils.GetInternalHelmReleaseName(addon.Name),
		Namespace: s.TargetNamespace,
	}, helmRelease); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("getting helm release: %w", err)
	}

	if helmRelease.Spec.Suspend == suspend {
		return nil
	}

	base := helmRelease.DeepCopy()
	helmRelease.Spec.Suspend = suspend

	if err := s.Client.Patch(ctx, helmRelease, client.MergeFrom(base)); err != nil {
		return fmt.Errorf("setting helm release suspend state: %w", err)
	}

	return nil
}
