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

package helmclusteraddonrepository

import (
	"context"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
	"github.com/deckhouse/operator-helm/internal/common"
	"github.com/deckhouse/operator-helm/internal/services"
	"github.com/deckhouse/operator-helm/internal/utils"
)

type reconciler struct {
	client.Client

	repositoryService *services.RepoService
	chartSyncService  *services.ChartSyncService
	statusManager     *services.StatusManager
}

func (r *reconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	logger := log.FromContext(ctx)
	ctx = log.IntoContext(ctx, logger)

	var repo helmv1alpha1.HelmClusterAddonRepository
	if err := r.Get(ctx, req.NamespacedName, &repo); err != nil {
		if apierrors.IsNotFound(err) {
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, fmt.Errorf("getting helm cluster addon repository: %w", err)
	}

	err := r.statusManager.InitializeConditions(ctx, &repo,
		services.ConditionTypeReady,
		services.ConditionTypeSynced,
	)
	if err != nil {
		return reconcile.Result{}, err
	}

	repoType, err := utils.GetRepositoryType(repo.Spec.URL)
	if err != nil {
		logger.Error(err, "failed to determine repository type")
		return reconcile.Result{}, err
	}

	if !repo.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, &repo, repoType)
	}

	if !controllerutil.ContainsFinalizer(&repo, FinalizerName) {
		controllerutil.AddFinalizer(&repo, FinalizerName)

		if err := r.Update(ctx, &repo); err != nil {
			return reconcile.Result{}, fmt.Errorf("adding finalizer: %w", err)
		}
		return r.requeueAtSyncInterval(&repo)
	}

	var repoRes services.RepoResult
	var chartSyncRes services.ChartSyncResult

	switch repoType {
	case utils.InternalHelmRepository:
		repoRes = r.repositoryService.EnsureInternalHelmRepository(ctx, &repo)
	case utils.InternalOCIRepository:
		// TODO: need to add extra check to ensure that URL provided by user is valid OCI url and credentials are correct.
		// Otherwise permanent ready status is invalid.

		// OCI repositories managed by helmclusteraddon controller. Parent object primary state is always true by default.
		repoRes = services.RepoResult{Status: services.Success(&repo)}
	default:
		err := fmt.Errorf("unsupported repository type: %q", repoType)
		repoRes = services.RepoResult{Status: services.Failed(&repo, "UnsupportedRepositoryType", err.Error(), err)}
	}

	if repoRes.Status.IsReady() {
		chartSyncRes = r.chartSyncService.EnsureAddonCharts(ctx, &repo, repoType)
	} else {
		chartSyncRes = services.ChartSyncResult{Status: services.Failed(&repo, services.ReasonRepositoryNotReady, repoRes.Status.Message, err)}
	}

	if err := r.statusManager.Update(ctx, &repo, repoRes, chartSyncRes); err != nil {
		return reconcile.Result{}, fmt.Errorf("failed to update status: %w", err)
	}

	return r.requeueAtSyncInterval(&repo)
}

func (r *reconciler) reconcileDelete(ctx context.Context, repo *helmv1alpha1.HelmClusterAddonRepository, repoType utils.InternalRepositoryType) (reconcile.Result, error) {
	logger := log.FromContext(ctx)

	if !controllerutil.ContainsFinalizer(repo, FinalizerName) {
		return reconcile.Result{}, nil
	}

	if repoType == utils.InternalHelmRepository {
		if err := r.repositoryService.InternalOCIRepositoryCleanup(ctx, repo); err != nil {
			_ = r.statusManager.Update(ctx, repo, services.RepoResult{
				Status: services.Failed(repo, common.ReasonFailed, "Failed to remove dependencies", err),
			})
			return reconcile.Result{}, err
		}
	}

	controllerutil.RemoveFinalizer(repo, FinalizerName)
	if err := r.Update(ctx, repo); err != nil {
		return reconcile.Result{}, fmt.Errorf("removing finalizer: %w", err)
	}

	logger.Info("Cleanup complete")

	return reconcile.Result{}, nil
}

func (r *reconciler) requeueAtSyncInterval(repo *helmv1alpha1.HelmClusterAddonRepository) (reconcile.Result, error) {
	repoSyncCond := apimeta.FindStatusCondition(repo.Status.Conditions, services.ConditionTypeSynced)
	if repoSyncCond != nil {
		remaining := time.Until(repoSyncCond.LastTransitionTime.Add(services.ChartsSyncInterval))
		if remaining > 0 {
			return reconcile.Result{RequeueAfter: remaining}, nil
		}
	}

	return reconcile.Result{RequeueAfter: services.ChartsSyncInterval}, nil
}
