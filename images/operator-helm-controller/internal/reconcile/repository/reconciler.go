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
	"github.com/deckhouse/operator-helm/internal/chartsource"
	"github.com/deckhouse/operator-helm/internal/contract"
	"github.com/deckhouse/operator-helm/internal/reconcile/lifecycle"
	"github.com/deckhouse/operator-helm/internal/status"
)

// Deps are the collaborators of one repository kind. NewRepository returns an empty adapter
// of that kind for the API object to be read into.
type Deps struct {
	NewRepository    func() contract.Repository
	Secrets          Secrets
	HelmRepositories HelmRepositories
	Consumers        contract.Consumers
	Catalogs         Catalogs
	Status           *status.Manager
}

func New(c client.Client, deps Deps) *Reconciler {
	return &Reconciler{Client: c, deps: deps}
}

type Reconciler struct {
	client.Client

	deps Deps
}

func (r *Reconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	repo := r.deps.NewRepository()
	if err := r.Get(ctx, req.NamespacedName, repo.Object()); err != nil {
		if apierrors.IsNotFound(err) {
			return reconcile.Result{}, nil
		}

		return reconcile.Result{}, fmt.Errorf("getting repository: %w", err)
	}

	repoType, repoTypeErr := chartsource.KindOf(repo.URL())

	if !repo.Object().GetDeletionTimestamp().IsZero() {
		return r.reconcileDelete(ctx, repo, repoType)
	}

	if !controllerutil.ContainsFinalizer(repo.Object(), helmv1alpha1.FinalizerName) {
		controllerutil.AddFinalizer(repo.Object(), helmv1alpha1.FinalizerName)

		if err := r.Update(ctx, repo.Object()); err != nil {
			return reconcile.Result{}, fmt.Errorf("adding finalizer: %w", err)
		}
		// Continue in the same pass: adding a finalizer bumps no generation, so no event follows.
	}

	in := Inputs{
		Generation: repo.Generation(),
		Now:        time.Now().UTC(),
		Jitter:     NewJitter(),
		Current:    *repo.Status().DeepCopy(),
	}

	// TRANSITIONAL: renamed here rather than inside the synchronization, to run even without a fetch.
	in.MigrateErr = r.deps.Catalogs.MigrateNames(ctx, repo)

	if repoTypeErr != nil {
		in.ConfigErr = &ConfigFailure{
			Reason:  helmv1alpha1.ReasonUnsupportedRepositoryType,
			Message: repoTypeErr.Error(),
			Err:     repoTypeErr,
		}

		return r.finish(ctx, repo, in, false)
	}

	in.SecretsErr = r.deps.Secrets.Ensure(ctx, repo, repoType)

	if in.SecretsErr == nil {
		switch repoType {
		case chartsource.Helm:
			in.InternalRepository, in.InternalRepositoryErr = r.deps.HelmRepositories.Ensure(ctx, repo)
		case chartsource.OCI:
			// The url may have moved from helm to oci; an oci repository has no internal object.
			in.InternalRepositoryErr = r.deps.HelmRepositories.Remove(ctx, repo.InternalNames())
		}
	}

	in.Forced = repo.ForceReconcileRequired()

	forceRecorded := false

	// An unfinished rename stops the synchronization: a pass needs exactly one object per chart.
	if in.MigrateErr == nil && in.SecretsErr == nil && in.InternalRepositoryErr == nil &&
		ShouldAttempt(in.Current, in.Generation, in.Now, in.Forced) {
		if err := r.markSyncInProgress(ctx, repo, in.Forced); err != nil {
			return reconcile.Result{}, err
		}

		outcome := r.deps.Catalogs.Sync(ctx, repo, repoType)
		forceRecorded = outcome.ForceRequestRecorded

		// Read after the synchronization, which may have taken the whole budget since in.Now.
		in.SyncEnd = time.Now().UTC()

		in.Attempted = true
		if outcome.FetchAttempted {
			// in.Fetch must stay nil otherwise: a zero Err would read as a successful fetch.
			in.Fetch = &outcome.Fetch
		}
		in.Catalog = &outcome.Catalog
	}

	return r.finish(ctx, repo, in, in.Forced && forceRecorded)
}

// finish applies the decision and consumes the force annotation once a forced attempt has
// recorded the request in the catalog. The annotation is removed after the status patch.
func (r *Reconciler) finish(
	ctx context.Context,
	repo contract.Repository,
	in Inputs,
	consumeForce bool,
) (reconcile.Result, error) {
	decision := Evaluate(in)

	if in.Fetch != nil && in.Fetch.Err != nil {
		// A read failure is not returned to the work queue, so this is the only place it is logged.
		log.FromContext(ctx).Error(in.Fetch.Err, in.Fetch.Message)
	}

	if err := r.deps.Status.PatchStatus(ctx, repo.Object(), func() {
		*repo.Status() = decision.Status
	}); client.IgnoreNotFound(err) != nil {
		return reconcile.Result{}, err
	}

	if consumeForce {
		// Run before the annotation is consumed, so a failure leaves the request to be retried.
		if repo.ForceReconcileRequired() {
			if err := r.deps.Consumers.ForceReconcileConsumers(ctx, repo); err != nil {
				return reconcile.Result{}, fmt.Errorf("forcing consumers to reconcile: %w", err)
			}
		}

		// Consumed even when the read failed: the catalog carries the request from here on.
		if err := lifecycle.ConsumeForceAnnotation(ctx, r.Client, client.ObjectKeyFromObject(repo.Object()), r.deps.NewRepository().Object()); err != nil {
			return reconcile.Result{}, fmt.Errorf("failed to reconcile force annotation: %w", err)
		}
	}

	if decision.Err != nil {
		// Cluster write failures go to the work queue rate limiter, not to the schedule.
		return reconcile.Result{}, decision.Err
	}

	return reconcile.Result{RequeueAfter: decision.RequeueAfter}, nil
}

func (r *Reconciler) reconcileDelete(ctx context.Context, repo contract.Repository, repoType chartsource.Kind) (reconcile.Result, error) {
	logger := log.FromContext(ctx)

	if !controllerutil.ContainsFinalizer(repo.Object(), helmv1alpha1.FinalizerName) {
		return reconcile.Result{}, nil
	}

	names := repo.InternalNames()

	if err := r.deps.Secrets.Cleanup(ctx, names); err != nil && !apierrors.IsNotFound(err) {
		_ = r.deps.Status.MarkDeletionFailed(ctx, repo.Object(), "auxiliary secrets", err)
		return reconcile.Result{}, err
	}

	// An unknown type takes the helm path too: the CRD's url regex is looser than url.Parse.
	if repoType != chartsource.OCI {
		helmRepo, err := r.deps.HelmRepositories.Cleanup(ctx, names)
		if err != nil && !apierrors.IsNotFound(err) {
			_ = r.deps.Status.MarkDeletionFailed(ctx, repo.Object(), "internal repository", err)
			return reconcile.Result{}, err
		}
		if helmRepo != nil {
			return lifecycle.AwaitInternalResourceDeletion(ctx, r.deps.Status.MarkDeletionPending, repo.Object(), "internal repository", helmRepo)
		}
	}

	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latest := r.deps.NewRepository()
		if err := r.Get(ctx, client.ObjectKeyFromObject(repo.Object()), latest.Object()); err != nil {
			return client.IgnoreNotFound(err)
		}

		if controllerutil.RemoveFinalizer(latest.Object(), helmv1alpha1.FinalizerName) {
			if err := r.Update(ctx, latest.Object()); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return reconcile.Result{}, fmt.Errorf("removing finalizer: %w", err)
	}

	logger.Info("Cleanup complete")

	return reconcile.Result{}, nil
}

// markSyncInProgress publishes Reconciling before the synchronization starts, with the reason
// naming which of the two triggers it was. It is written outside the Inputs snapshot.
func (r *Reconciler) markSyncInProgress(
	ctx context.Context,
	repo contract.Repository,
	forced bool,
) error {
	reason, message := helmv1alpha1.ReasonSynchronization, "Repository synchronization in progress"
	if forced {
		reason, message = helmv1alpha1.ReasonForceReconcile, "Forced reconciliation in progress"
	}

	err := r.deps.Status.PatchStatus(ctx, repo.Object(), func() {
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
