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

	repoType, repoTypeErr := utils.GetRepositoryType(repo.Spec.URL)

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

	in := Inputs{
		Generation: repo.Generation,
		Now:        time.Now().UTC(),
		Jitter:     NewJitter(),
		Current:    *repo.Status.DeepCopy(),
	}

	if repoTypeErr != nil {
		in.ConfigErr = &services.ConfigOutcome{
			Reason:  helmv1alpha1.ReasonUnsupportedRepositoryType,
			Message: repoTypeErr.Error(),
			Err:     repoTypeErr,
		}

		return r.finish(ctx, &repo, in, false)
	}

	// Both services embed the same BaseRepoService with the same target namespace,
	// so one of them reconciles the auxiliary secrets for either repository type.
	in.SecretsErr = r.helmRepositoryService.EnsureSecrets(ctx, &repo)

	if in.SecretsErr == nil {
		switch repoType {
		case utils.InternalHelmRepository:
			in.InternalRepository, in.InternalRepositoryErr = r.helmRepositoryService.EnsureInternalHelmRepository(ctx, &repo)
		case utils.InternalOCIRepository:
			// The url may have changed from helm to oci: drop the internal object
			// that is no longer used. OCI repositories have none of their own.
			in.InternalRepositoryErr = r.helmRepositoryService.RemoveHelmRepository(ctx, repo.Name)
		}
	}

	forced := repo.ForceReconcileRequired()

	if in.SecretsErr == nil && in.InternalRepositoryErr == nil &&
		ShouldAttempt(in.Current, in.Generation, in.Now, forced) {
		outcome := r.chartSyncService.Sync(ctx, &repo, repoType)

		in.Attempted = true
		in.Fetch = &outcome.Fetch
		in.Catalog = &outcome.Catalog
	}

	return r.finish(ctx, &repo, in, in.Attempted)
}

// finish applies the decision and consumes the force annotation when an attempt
// actually ran. The annotation is removed after the status patch so a conflict
// does not lose the request.
func (r *Reconciler) finish(
	ctx context.Context,
	repo *helmv1alpha1.HelmClusterAddonRepository,
	in Inputs,
	attempted bool,
) (reconcile.Result, error) {
	decision := Evaluate(in)

	if err := r.statusManager.PatchStatus(ctx, repo, func() {
		repo.Status = decision.Status
	}); client.IgnoreNotFound(err) != nil {
		return reconcile.Result{}, fmt.Errorf("failed to update status: %w", err)
	}

	if attempted {
		if err := r.reconcileForceAnnotation(ctx, client.ObjectKeyFromObject(repo)); err != nil {
			return reconcile.Result{}, fmt.Errorf("failed to reconcile force annotation: %w", err)
		}
	}

	if decision.Err != nil {
		// Cluster write failures are handed to the work queue rate limiter; the
		// schedule is re-established on the next pass.
		return reconcile.Result{}, decision.Err
	}

	return reconcile.Result{RequeueAfter: decision.RequeueAfter}, nil
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

func (r *Reconciler) reconcileForceAnnotation(ctx context.Context, key client.ObjectKey) error {
	var repo helmv1alpha1.HelmClusterAddonRepository

	if err := r.Get(ctx, key, &repo); err != nil {
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
