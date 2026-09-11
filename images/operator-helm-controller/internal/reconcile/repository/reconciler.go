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

package repository

import (
	"context"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
	"github.com/deckhouse/operator-helm/internal/manager/status"
	"github.com/deckhouse/operator-helm/internal/services"
	"github.com/deckhouse/operator-helm/internal/source"
	"github.com/deckhouse/operator-helm/internal/utils"
)

// internalResourceDeletionRequeueInterval bounds how often reconcileDelete
// re-checks whether the internal resources have finished being deleted. Watches
// on those resources drive most requeues; this is the safety net for a resource
// whose deletion is stuck and stops emitting events.
const internalResourceDeletionRequeueInterval = 30 * time.Second

// New builds the reconciler of one repository kind. newRepository returns an
// empty adapter of that kind for the API object to be read into; consumers pushes
// a force request onto the internal sources of whatever consumes the repository
// in that family.
func New(
	client client.Client,
	newRepository func() source.Repository,
	helmRepositoryService *services.HelmRepoService,
	ociRepositoryService *services.OCIRepoService,
	consumers source.ConsumerForcer,
	chartSyncService *services.RepoSyncService,
	statusManager *status.Manager,
) *Reconciler {
	return &Reconciler{
		Client:                client,
		newRepository:         newRepository,
		helmRepositoryService: helmRepositoryService,
		ociRepositoryService:  ociRepositoryService,
		consumers:             consumers,
		chartSyncService:      chartSyncService,
		statusManager:         statusManager,
	}
}

type Reconciler struct {
	client.Client

	newRepository         func() source.Repository
	helmRepositoryService *services.HelmRepoService
	ociRepositoryService  *services.OCIRepoService
	consumers             source.ConsumerForcer
	chartSyncService      *services.RepoSyncService
	statusManager         *status.Manager
}

func (r *Reconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	logger := log.FromContext(ctx)
	ctx = log.IntoContext(ctx, logger)

	repo := r.newRepository()
	if err := r.Get(ctx, req.NamespacedName, repo.Object()); err != nil {
		if apierrors.IsNotFound(err) {
			return reconcile.Result{}, nil
		}

		return reconcile.Result{}, fmt.Errorf("getting repository: %w", err)
	}

	repoType, repoTypeErr := utils.GetRepositoryType(repo.URL())

	if !repo.Object().GetDeletionTimestamp().IsZero() {
		return r.reconcileDelete(ctx, repo, repoType)
	}

	if !controllerutil.ContainsFinalizer(repo.Object(), helmv1alpha1.FinalizerName) {
		controllerutil.AddFinalizer(repo.Object(), helmv1alpha1.FinalizerName)

		if err := r.Update(ctx, repo.Object()); err != nil {
			return reconcile.Result{}, fmt.Errorf("adding finalizer: %w", err)
		}
		// Continue reconciling in the same pass: adding a finalizer is a
		// metadata-only change that does not bump generation, so the resulting
		// update event is dropped by the generation/annotation predicates and
		// would not trigger a follow-up reconcile.
	}

	// TRANSITIONAL: the catalog object names moved, and a consumer resolves the new
	// name from the moment this controller starts. Renaming here rather than inside
	// the synchronization keeps it independent of the fetch: a repository that is not
	// due for a sync yet, or whose remote is gone for good, still gets its objects
	// moved.
	if err := r.chartSyncService.MigrateNames(ctx, repo); err != nil {
		return reconcile.Result{}, fmt.Errorf("migrating chart catalog names: %w", err)
	}

	in := Inputs{
		Generation: repo.Generation(),
		Now:        time.Now().UTC(),
		Jitter:     NewJitter(),
		Current:    *repo.Status().DeepCopy(),
	}

	if repoTypeErr != nil {
		in.ConfigErr = &services.ConfigOutcome{
			Reason:  helmv1alpha1.ReasonUnsupportedRepositoryType,
			Message: repoTypeErr.Error(),
			Err:     repoTypeErr,
		}

		return r.finish(ctx, repo, in, false)
	}

	// Both services embed the same BaseRepoService with the same target namespace,
	// so one of them reconciles the auxiliary secrets for either repository type.
	in.SecretsErr = r.helmRepositoryService.EnsureSecrets(ctx, repo, repoType)

	if in.SecretsErr == nil {
		switch repoType {
		case utils.InternalHelmRepository:
			in.InternalRepository, in.InternalRepositoryErr = r.helmRepositoryService.EnsureInternalHelmRepository(ctx, repo)
		case utils.InternalOCIRepository:
			// The url may have changed from helm to oci: drop the internal object
			// that is no longer used. OCI repositories have none of their own.
			in.InternalRepositoryErr = r.helmRepositoryService.RemoveHelmRepository(ctx, repo.InternalNames())
		}
	}

	in.Forced = repo.ForceReconcileRequired()

	if in.SecretsErr == nil && in.InternalRepositoryErr == nil &&
		ShouldAttempt(in.Current, in.Generation, in.Now, in.Forced) {
		if err := r.markSyncInProgress(ctx, repo, in.Forced); err != nil {
			return reconcile.Result{}, err
		}

		outcome := r.chartSyncService.Sync(ctx, repo, repoType)

		in.Attempted = true
		if outcome.FetchAttempted {
			// A cluster-side failure before the fetch (see RepoSyncService.Sync)
			// leaves outcome.FetchAttempted false; in.Fetch must stay nil then, or
			// its zero-value Err == nil would be read as a successful fetch and
			// reset ConsecutiveFetchFailures / mark Ready=True off nothing.
			in.Fetch = &outcome.Fetch
		}
		in.Catalog = &outcome.Catalog
	}

	return r.finish(ctx, repo, in, in.Attempted)
}

// finish applies the decision and consumes the force annotation when an attempt
// actually ran. The annotation is removed after the status patch so a conflict
// does not lose the request.
func (r *Reconciler) finish(
	ctx context.Context,
	repo source.Repository,
	in Inputs,
	attempted bool,
) (reconcile.Result, error) {
	decision := Evaluate(in)

	if in.Fetch != nil && in.Fetch.Err != nil {
		// A repository read failure is not returned to the work queue — its retry
		// is carried by nextSyncTime — so this is the only place it is logged.
		log.FromContext(ctx).Error(in.Fetch.Err, in.Fetch.Message, "repository", repo.Name())
	}

	if err := r.statusManager.PatchStatus(ctx, repo.Object(), func() {
		*repo.Status() = decision.Status
	}); client.IgnoreNotFound(err) != nil {
		return reconcile.Result{}, err
	}

	if attempted {
		// A force request reaches a consumer's artifact only through the consumer's
		// own internal OCIRepository, and any repository can have those: an oci:// one
		// for every consumer, a helm one for every version its index publishes in a
		// registry. This runs before the annotation is consumed: a failure leaves the
		// request in place to be retried. The versions a helm repository serves as
		// archives need no equivalent — there the internal HelmRepository carries the
		// request and its HelmCharts follow the re-indexed source on their own.
		if repo.ForceReconcileRequired() {
			if err := r.consumers.ForceReconcileConsumers(ctx, repo); err != nil {
				return reconcile.Result{}, fmt.Errorf("failed to force reconcile internal oci repositories: %w", err)
			}
		}

		if err := r.reconcileForceAnnotation(ctx, client.ObjectKeyFromObject(repo.Object())); err != nil {
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

func (r *Reconciler) reconcileDelete(ctx context.Context, repo source.Repository, repoType utils.InternalRepositoryType) (reconcile.Result, error) {
	logger := log.FromContext(ctx)

	if !controllerutil.ContainsFinalizer(repo.Object(), helmv1alpha1.FinalizerName) {
		return reconcile.Result{}, nil
	}

	names := repo.InternalNames()

	switch repoType {
	case utils.InternalOCIRepository:
		if err := r.ociRepositoryService.CleanupOCIRepository(ctx, names); err != nil && !apierrors.IsNotFound(err) {
			_ = r.statusManager.MarkDeletionFailed(ctx, repo.Object(), "internal repository", err)
			return reconcile.Result{}, err
		}
	default:
		// The helm path is the default rather than a case of its own because an
		// unknown repository type is a state a real repository can reach: the url
		// validation regex on the CRD is looser than url.Parse, so a repository
		// whose internal objects already exist can be edited to a url that no
		// longer parses and then deleted. Cleaning up the helm way is safe for
		// either type — it removes both auxiliary secrets and tolerates a missing
		// internal repository — and leaving it out would orphan them.
		helmRepo, err := r.helmRepositoryService.CleanupHelmRepository(ctx, names)
		if err != nil && !apierrors.IsNotFound(err) {
			_ = r.statusManager.MarkDeletionFailed(ctx, repo.Object(), "internal repository", err)
			return reconcile.Result{}, err
		}
		if helmRepo != nil {
			return r.awaitInternalResourceDeletion(ctx, repo, "internal repository", helmRepo)
		}
	}

	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latest := r.newRepository()
		if err := r.Get(ctx, client.ObjectKeyFromObject(repo.Object()), latest.Object()); err != nil {
			return client.IgnoreNotFound(err)
		}

		if controllerutil.RemoveFinalizer(latest.Object(), helmv1alpha1.FinalizerName) {
			if err := r.Update(ctx, latest.Object()); err != nil {
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
func (r *Reconciler) awaitInternalResourceDeletion(ctx context.Context, repo source.Repository, name string, resource status.DeletingResource) (reconcile.Result, error) {
	log.FromContext(ctx).Info("Waiting for internal resource to be deleted before removing finalizer", "resource", name)

	if err := r.statusManager.MarkDeletionPending(ctx, repo.Object(), name, resource); client.IgnoreNotFound(err) != nil {
		return reconcile.Result{}, fmt.Errorf("updating deletion status: %w", err)
	}

	return reconcile.Result{RequeueAfter: internalResourceDeletionRequeueInterval}, nil
}

// markSyncInProgress publishes Reconciling before the synchronization starts, so
// a pass that is about to read the repository says so while the read is running
// instead of only once it is over — a read can take a while, and until it
// returns nothing else on the status moves. The reason distinguishes the two ways
// a pass is triggered: ForceReconcile is the case someone is actively watching,
// having annotated the repository a moment ago to see it picked up, while
// Synchronization is the ordinary scheduled cadence. The condition is
// deliberately written outside the Inputs snapshot Evaluate works from, so the
// status computed at the end of the pass removes it again without a rule of its
// own.
func (r *Reconciler) markSyncInProgress(
	ctx context.Context,
	repo source.Repository,
	forced bool,
) error {
	reason, message := helmv1alpha1.ReasonSynchronization, "Repository synchronization in progress"
	if forced {
		reason, message = helmv1alpha1.ReasonForceReconcile, "Forced reconciliation in progress"
	}

	err := r.statusManager.PatchStatus(ctx, repo.Object(), func() {
		apimeta.SetStatusCondition(&repo.Status().Conditions, metav1.Condition{
			Type:               helmv1alpha1.ConditionTypeReconciling,
			Status:             metav1.ConditionTrue,
			Reason:             reason,
			Message:            message,
			ObservedGeneration: repo.Generation(),
		})
	})
	if client.IgnoreNotFound(err) != nil {
		return fmt.Errorf("publishing synchronization progress: %w", err)
	}

	return nil
}

func (r *Reconciler) reconcileForceAnnotation(ctx context.Context, key client.ObjectKey) error {
	repo := r.newRepository()

	if err := r.Get(ctx, key, repo.Object()); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}

		return fmt.Errorf("getting repository: %w", err)
	}

	annotations := repo.Object().GetAnnotations()
	if _, found := annotations[helmv1alpha1.AnnotationForceReconcile]; !found {
		// Guard on the annotation itself, not on the map: a repository carrying
		// any unrelated annotation would otherwise take an empty PATCH on every
		// attempted pass.
		return nil
	}

	patchBase := client.MergeFrom(repo.Object().DeepCopyObject().(client.Object))

	delete(annotations, helmv1alpha1.AnnotationForceReconcile)
	repo.Object().SetAnnotations(annotations)

	if err := r.Patch(ctx, repo.Object(), patchBase); err != nil {
		return fmt.Errorf("removing force reconcile annotation: %w", err)
	}

	return nil
}
