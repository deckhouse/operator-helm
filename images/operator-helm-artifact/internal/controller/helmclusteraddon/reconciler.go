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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
	"github.com/deckhouse/operator-helm/internal/common"
	"github.com/deckhouse/operator-helm/internal/services"
	"github.com/deckhouse/operator-helm/internal/utils"
)

const (
	ConditionTypeManaged              = "Managed"
	ConditionTypeInstalled            = "Installed"
	ConditionTypeUpdateInstalled      = "UpdateInstalled"
	ConditionTypeConfigurationApplied = "ConfigurationApplied"
	ConditionTypePartiallyDegraded    = "PartiallyDegraded"
)

type reconciler struct {
	client.Client

	chartService      *services.ChartService
	repositoryService *services.RepoService
	releaseService    *services.ReleaseService
	statusManager     *services.StatusManager
}

func (r *reconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	logger := log.FromContext(ctx)
	ctx = log.IntoContext(ctx, logger)

	addon := &helmv1alpha1.HelmClusterAddon{}
	if err := r.Get(ctx, req.NamespacedName, addon); err != nil {
		if apierrors.IsNotFound(err) {
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, fmt.Errorf("getting HelmClusterAddon: %w", err)
	}

	err := r.statusManager.InitializeConditions(ctx, addon,
		common.ConditionTypeReady,
		ConditionTypeManaged,
		ConditionTypeInstalled,
		ConditionTypeUpdateInstalled,
		ConditionTypeConfigurationApplied,
		ConditionTypePartiallyDegraded,
	)
	if err != nil {
		return reconcile.Result{}, err
	}

	if !addon.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, addon)
	}

	if !controllerutil.ContainsFinalizer(addon, helmv1alpha1.FinalizerName) {
		controllerutil.AddFinalizer(addon, helmv1alpha1.FinalizerName)
		if err := r.Update(ctx, addon); err != nil {
			return reconcile.Result{}, fmt.Errorf("adding finalizer: %w", err)
		}
		return reconcile.Result{}, nil
	}

	if addon.MaintenanceModeEnabled() {
		releaseRes := r.releaseService.SuspendHelmRelease(ctx, addon)
		return reconcile.Result{}, r.statusManager.Update(ctx, addon, services.NoopStatusMutator, releaseRes)
	}

	repo := &helmv1alpha1.HelmClusterAddonRepository{}
	if err := r.Get(ctx, types.NamespacedName{Name: addon.Spec.Chart.HelmClusterAddonRepository}, repo); err != nil {
		return reconcile.Result{}, r.statusManager.Update(ctx, addon, services.NoopStatusMutator, services.ReleaseResult{Status: services.Failed(
			addon,
			common.ReasonFailed,
			"Failed to get internal repository",
			fmt.Errorf("getting internal repository: %w", err),
		)})
	}

	repoType, err := utils.GetRepositoryType(repo.Spec.URL)
	if err != nil {
		return reconcile.Result{}, r.statusManager.Update(ctx, addon, services.NoopStatusMutator, services.ReleaseResult{Status: services.Failed(
			addon,
			common.ReasonFailed,
			fmt.Sprintf("Failed to parse repository type: %s", err.Error()),
			err,
		)})
	}

	var chartRes services.ChartResult
	var partiallyDegradedRes services.PartiallyDegradedResult
	var repoRes services.RepoResult
	var releaseRes services.ReleaseResult

	switch repoType {
	case utils.InternalHelmRepository:
		chartRes = r.chartService.EnsureHelmChart(ctx, addon, repo)
		// If there is a problem with getting fresh chart artifact, but we still have previous one -
		// add PartiallyDegraded condition type.
		if chartRes.IsPartiallyDegraded() {
			partiallyDegradedRes = services.PartiallyDegradedResult{Status: services.Failed(
				addon,
				chartRes.Status.Reason,
				chartRes.Status.Message,
				chartRes.Status.Err,
			)}
		}
	case utils.InternalOCIRepository:
		repoRes = r.repositoryService.EnsureInternalOCIRepository(ctx, repo)
	default:
		return reconcile.Result{}, r.statusManager.Update(ctx, addon, services.NoopStatusMutator, services.ReleaseResult{Status: services.Failed(
			addon,
			common.ReasonFailed,
			fmt.Sprintf("Unsupported repository type: %s", repoType),
			err,
		)})
	}

	if chartRes.IsReady() || repoRes.IsReady() {
		releaseRes = r.releaseService.EnsureHelmRelease(ctx, addon, repoType)
	}

	if !releaseRes.IsReady() {
		releaseRes = services.ReleaseResult{Status: services.Unknown(addon, common.ReasonReconciling)}
	}

	consolidatedRes := services.ConsolidateConditions(addon, chartRes, repoRes, releaseRes)
	consolidatedRes = append(consolidatedRes, partiallyDegradedRes)

	return reconcile.Result{}, r.statusManager.Update(ctx, addon, services.NoopStatusMutator, consolidatedRes...)
}

func (r *reconciler) reconcileDelete(ctx context.Context, addon *helmv1alpha1.HelmClusterAddon) (reconcile.Result, error) {
	logger := log.FromContext(ctx)

	if !controllerutil.ContainsFinalizer(addon, helmv1alpha1.FinalizerName) {
		return reconcile.Result{}, nil
	}

	// TODO: probably need to perform action only for exact repoType

	if err := r.repositoryService.CleanupOCIRepository(ctx, addon.Spec.Chart.HelmClusterAddonRepository); err != nil && !apierrors.IsNotFound(err) {
		return reconcile.Result{}, err
	}

	if err := r.repositoryService.CleanupHelmRepository(ctx, addon.Spec.Chart.HelmClusterAddonRepository); err != nil && !apierrors.IsNotFound(err) {
		return reconcile.Result{}, err
	}

	if err := r.chartService.CleanupHelmChart(ctx, addon); err != nil && !apierrors.IsNotFound(err) {
		return reconcile.Result{}, err
	}

	if err := r.releaseService.CleanupHelmRelease(ctx, addon); err != nil && !apierrors.IsNotFound(err) {
		return reconcile.Result{}, err
	}

	controllerutil.RemoveFinalizer(addon, helmv1alpha1.FinalizerName)
	if err := r.Update(ctx, addon); err != nil && !apierrors.IsNotFound(err) {
		return reconcile.Result{}, fmt.Errorf("removing finalizer: %w", err)
	}

	logger.Info("Cleanup complete")

	return reconcile.Result{}, nil
}
