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
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
	"github.com/deckhouse/operator-helm/internal/manager/status"
	"github.com/deckhouse/operator-helm/internal/services"
	"github.com/deckhouse/operator-helm/internal/utils"
)

// internalResourceDeletionRequeueInterval bounds how often reconcileDelete
// re-checks whether the internal resources have finished being deleted. Watches
// on those resources drive most requeues; this is the safety net for a resource
// whose deletion is stuck and stops emitting events.
const internalResourceDeletionRequeueInterval = 30 * time.Second

func New(
	client client.Client,
	helmRepositoryService *services.HelmRepoService,
	ociRepositoryService *services.OCIRepoService,
	chartSyncService *services.RepoSyncService,
	statusManager *status.Manager,
) *Reconciler {
	return &Reconciler{
		Client:                client,
		helmRepositoryService: helmRepositoryService,
		ociRepositoryService:  ociRepositoryService,
		chartSyncService:      chartSyncService,
		statusManager:         statusManager,
	}
}

type Reconciler struct {
	client.Client

	helmRepositoryService *services.HelmRepoService
	ociRepositoryService  *services.OCIRepoService
	chartSyncService      *services.RepoSyncService
	statusManager         *status.Manager
}

func (r *Reconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	logger := log.FromContext(ctx)
	ctx = log.IntoContext(ctx, logger)

	var repo helmv1alpha1.HelmClusterAddonRepository
	if err := r.Get(ctx, req.NamespacedName, &repo); err != nil {
		if apierrors.IsNotFound(err) {
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, fmt.Errorf("getting helm cluster addon repository: %w", err)
	}

	repoType, err := utils.GetRepositoryType(repo.Spec.URL)
	if err != nil {
		logger.Error(err, "failed to determine repository type")
		return reconcile.Result{}, err
	}

	if !repo.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, &repo, repoType)
	}

	if !controllerutil.ContainsFinalizer(&repo, helmv1alpha1.FinalizerName) {
		controllerutil.AddFinalizer(&repo, helmv1alpha1.FinalizerName)

		if err := r.Update(ctx, &repo); err != nil {
			return reconcile.Result{}, fmt.Errorf("adding finalizer: %w", err)
		}
		// Continue reconciling in the same pass: adding a finalizer is a
		// metadata-only change that does not bump generation, so the resulting
		// update event is dropped by the generation/annotation predicates and
		// would not trigger a follow-up reconcile.
	}

	var helmRepoRes services.HelmRepoResult
	var ociRepoRes services.OCIRepoResult
	var chartSyncRes services.RepoSyncResult

	switch repoType {
	case utils.InternalHelmRepository:
		helmRepoRes = r.helmRepositoryService.EnsureInternalHelmRepository(ctx, &repo)
	case utils.InternalOCIRepository:
		if err := r.helmRepositoryService.RemoveHelmRepository(ctx, repo.Name); err != nil {
			ociRepoRes = services.OCIRepoResult{
				Status: status.Failed(&repo, helmv1alpha1.ReasonFailed, "Repository change failed", err),
			}
			break
		}
		ociRepoRes = r.ociRepositoryService.EnsureRepositorySecrets(ctx, &repo)
	default:
		err := fmt.Errorf("unsupported repository type: %q", repoType)
		helmRepoRes = services.HelmRepoResult{Status: status.Failed(&repo, "UnsupportedRepositoryType", err.Error(), err)}
	}

	if helmRepoRes.IsReady() || ociRepoRes.IsReady() {
		chartSyncRes = r.chartSyncService.EnsureAddonCharts(ctx, &repo, repoType)
	} else {
		chartSyncRes = services.RepoSyncResult{Status: status.Failed(&repo, helmv1alpha1.ReasonRepositoryNotReady, helmRepoRes.Status.Message, nil)}
	}

	if err := r.reconcileForceAnnotation(ctx, req); err != nil {
		return reconcile.Result{}, fmt.Errorf("failed to reconcile force annotation: %w", err)
	}

	if err := r.statusManager.Update(
		ctx,
		&repo,
		status.NoopStatusMutator,
		status.NoopStatusMapper,
		helmRepoRes,
		ociRepoRes,
		chartSyncRes,
	); client.IgnoreNotFound(err) != nil {
		return reconcile.Result{}, fmt.Errorf("failed to update status: %w", err)
	}

	// EnsureAddonCharts is a two-phase state machine: the first pass only marks
	// the Synced condition Reconciling, the second pass performs the actual chart
	// fetch. Run the second pass in the same reconcile (the status update above
	// already persisted the Reconciling state and advanced the condition's
	// LastTransitionTime) instead of relying on the status-update watch event to
	// trigger it — otherwise predicates that ignore status-only changes would
	// stall the scheduled sync.
	if chartSyncRes.InProgress() {
		chartSyncRes = r.chartSyncService.EnsureAddonCharts(ctx, &repo, repoType)
		if err := r.statusManager.Update(
			ctx,
			&repo,
			status.NoopStatusMutator,
			status.NoopStatusMapper,
			chartSyncRes,
		); client.IgnoreNotFound(err) != nil {
			return reconcile.Result{}, fmt.Errorf("failed to update sync status: %w", err)
		}
	}

	return r.requeueAtSyncInterval(&repo)
}

func (r *Reconciler) reconcileDelete(ctx context.Context, repo *helmv1alpha1.HelmClusterAddonRepository, repoType utils.InternalRepositoryType) (reconcile.Result, error) {
	logger := log.FromContext(ctx)

	if !controllerutil.ContainsFinalizer(repo, helmv1alpha1.FinalizerName) {
		return reconcile.Result{}, nil
	}

	switch repoType {
	case utils.InternalHelmRepository:
		helmRepo, err := r.helmRepositoryService.CleanupHelmRepository(ctx, repo.Name)
		if err != nil && !apierrors.IsNotFound(err) {
			_ = r.statusManager.MarkDeletionFailed(ctx, repo, "internal repository", err)
			return reconcile.Result{}, err
		}
		if helmRepo != nil {
			return r.awaitInternalResourceDeletion(ctx, repo, "internal repository", helmRepo)
		}
	case utils.InternalOCIRepository:
		if err := r.ociRepositoryService.CleanupOCIRepository(ctx, repo.Name); err != nil && !apierrors.IsNotFound(err) {
			_ = r.statusManager.MarkDeletionFailed(ctx, repo, "internal repository", err)
			return reconcile.Result{}, err
		}
	}

	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latestRepo := &helmv1alpha1.HelmClusterAddonRepository{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(repo), latestRepo); err != nil {
			return client.IgnoreNotFound(err)
		}

		if controllerutil.RemoveFinalizer(latestRepo, helmv1alpha1.FinalizerName) {
			if err := r.Update(ctx, latestRepo); err != nil {
				return err // This will trigger a retry if it's a conflict
			}
		}
		return nil
	}); err != nil {
		return reconcile.Result{}, fmt.Errorf("removing finalizer: %w", err)
	}

	logger.Info("Cleanup complete")

	return reconcile.Result{}, nil
}

// awaitInternalResourceDeletion surfaces that an internal resource is still being
// deleted on the repository's status (via the shared status manager) and requeues
// without removing the finalizer. The resource name is kept abstract so its
// internal type is not leaked to the user.
func (r *Reconciler) awaitInternalResourceDeletion(ctx context.Context, repo *helmv1alpha1.HelmClusterAddonRepository, name string, resource status.DeletingResource) (reconcile.Result, error) {
	log.FromContext(ctx).Info("Waiting for internal resource to be deleted before removing finalizer", "resource", name)

	if err := r.statusManager.MarkDeletionPending(ctx, repo, name, resource); client.IgnoreNotFound(err) != nil {
		return reconcile.Result{}, fmt.Errorf("updating deletion status: %w", err)
	}

	return reconcile.Result{RequeueAfter: internalResourceDeletionRequeueInterval}, nil
}

func (r *Reconciler) reconcileForceAnnotation(ctx context.Context, req reconcile.Request) error {
	var repo helmv1alpha1.HelmClusterAddonRepository

	if err := r.Get(ctx, req.NamespacedName, &repo); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("getting helm cluster addon repository: %w", err)
	}

	if repo.Annotations == nil {
		return nil
	}

	patchBase := client.MergeFrom(repo.DeepCopy())

	delete(repo.Annotations, helmv1alpha1.AnnotationForceReconcile)

	if err := r.Patch(ctx, &repo, patchBase); err != nil {
		return fmt.Errorf("removing force reconcile annotation: %w", err)
	}

	return nil
}

func (r *Reconciler) requeueAtSyncInterval(repo *helmv1alpha1.HelmClusterAddonRepository) (reconcile.Result, error) {
	repoSyncCond := apimeta.FindStatusCondition(repo.Status.Conditions, helmv1alpha1.ConditionTypeSynced)
	if repoSyncCond != nil {
		remaining := time.Until(repoSyncCond.LastTransitionTime.Add(services.ChartsSyncInterval))
		if remaining > 0 {
			return reconcile.Result{RequeueAfter: remaining}, nil
		}
	}

	return reconcile.Result{RequeueAfter: services.ChartsSyncInterval}, nil
}
