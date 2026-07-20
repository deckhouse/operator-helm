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
	"time"

	"github.com/opencontainers/go-digest"
	"github.com/werf/3p-fluxcd-pkg/chartutil"
	helmchartutil "helm.sh/helm/v3/pkg/chartutil"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
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

// chartClaimConflictRequeueInterval bounds how often an addon that lost the claim
// on its repository/chart pair re-checks whether the owner has released it. There
// is no watch on the claim Lease, so this periodic requeue is what lets a duplicate
// recover once the conflicting addon is deleted or repointed at another chart.
const chartClaimConflictRequeueInterval = 30 * time.Second

func New(
	client client.Client,
	chartService *services.ChartService,
	ociRepositoryService *services.OCIRepoService,
	releaseService *services.ReleaseService,
	maintenanceService *services.MaintenanceService,
	claimService *services.ClaimService,
	statusManager *status.Manager,
) *Reconciler {
	return &Reconciler{
		Client:               client,
		chartService:         chartService,
		ociRepositoryService: ociRepositoryService,
		releaseService:       releaseService,
		maintenanceService:   maintenanceService,
		claimService:         claimService,
		statusManager:        statusManager,
	}
}

type Reconciler struct {
	client.Client

	chartService         *services.ChartService
	ociRepositoryService *services.OCIRepoService
	releaseService       *services.ReleaseService
	maintenanceService   *services.MaintenanceService
	claimService         *services.ClaimService
	statusManager        *status.Manager
}

func (r *Reconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	logger := log.FromContext(ctx)
	ctx = log.IntoContext(ctx, logger)

	addon := &helmv1alpha1.HelmClusterAddon{}
	if err := r.Get(ctx, req.NamespacedName, addon); err != nil {
		if apierrors.IsNotFound(err) {
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, fmt.Errorf("getting helm cluster addon: %w", err)
	}

	if !addon.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, addon)
	}

	// Claim the repository/chart pair before anything else, including adding the
	// finalizer. The claim is the authoritative, race-free guard on uniqueness (the
	// webhook only fast-rejects the obvious duplicate on CREATE and cannot stop
	// concurrent creates from racing past it). It must run before the finalizer
	// because a duplicate that loses the race must not accrue a finalizer it would
	// otherwise have to clean up: it simply surfaces the conflict on its status and
	// requeues, recovering on its own once the owner releases the pair.
	acquired, holder, err := r.claimService.Acquire(ctx, addon)
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("acquiring chart claim: %w", err)
	}
	if !acquired {
		return reconcile.Result{RequeueAfter: chartClaimConflictRequeueInterval}, r.statusManager.Update(
			ctx, addon, status.NoopStatusMutator, status.NoopStatusMapper,
			services.ReleaseResult{Status: status.Failed(
				addon,
				helmv1alpha1.ReasonChartClaimConflict,
				fmt.Sprintf("chart %q is already used by helmclusteraddon/%s", addon.Spec.Chart.HelmClusterAddonChartName, holder),
				nil,
			)},
		)
	}

	if !controllerutil.ContainsFinalizer(addon, helmv1alpha1.FinalizerName) {
		controllerutil.AddFinalizer(addon, helmv1alpha1.FinalizerName)
		if err := r.Update(ctx, addon); err != nil {
			return reconcile.Result{}, fmt.Errorf("adding finalizer: %w", err)
		}
		// Continue reconciling in the same pass: adding a finalizer is a
		// metadata-only change that does not bump generation, so the resulting
		// update event is dropped by the generation/annotation predicates and
		// would not trigger a follow-up reconcile.
	}

	if err := r.claimService.ReleaseStale(ctx, addon); err != nil {
		return reconcile.Result{}, fmt.Errorf("releasing stale chart claims: %w", err)
	}

	if r.maintenanceService.IsMaintenanceModeChangeRequired(addon) {
		maintenanceRes := r.maintenanceService.EnsureMaintenanceMode(ctx, addon)
		return reconcile.Result{}, r.statusManager.Update(ctx, addon, status.NoopStatusMutator, status.NoopStatusMapper, maintenanceRes, status.AsCondition(maintenanceRes, "Ready"))
	}

	if addon.MaintenanceModeActivated() {
		return reconcile.Result{}, nil
	}

	repo := &helmv1alpha1.HelmClusterAddonRepository{}
	if err := r.Get(ctx, types.NamespacedName{Name: addon.Spec.Chart.HelmClusterAddonRepository}, repo); err != nil {
		return reconcile.Result{}, r.statusManager.Update(ctx, addon, status.NoopStatusMutator, status.NoopStatusMapper, services.ReleaseResult{Status: status.Failed(
			addon,
			helmv1alpha1.ReasonFailed,
			"Failed to get internal repository",
			fmt.Errorf("getting internal repository: %w", err),
		)})
	}

	repoType, err := utils.GetRepositoryType(repo.Spec.URL)
	if err != nil {
		return reconcile.Result{}, r.statusManager.Update(ctx, addon, status.NoopStatusMutator, status.NoopStatusMapper, services.ReleaseResult{Status: status.Failed(
			addon,
			helmv1alpha1.ReasonFailed,
			fmt.Sprintf("Failed to parse repository type: %s", err.Error()),
			err,
		)})
	}

	if err := r.reconcileAddonNamespace(ctx, addon); err != nil {
		return reconcile.Result{}, r.statusManager.Update(ctx, addon, status.NoopStatusMutator, status.NoopStatusMapper, services.ReleaseResult{Status: status.Failed(
			addon,
			helmv1alpha1.ReasonFailed,
			fmt.Sprintf("Failed to reconcile target namespace: %s", err.Error()),
			err,
		)})
	}

	var chartRes services.ChartResult
	var repoRes services.OCIRepoResult
	var releaseRes services.ReleaseResult

	_, addonChartErr := r.getHelmClusterAddonChart(ctx, addon)

	switch repoType {
	case utils.InternalHelmRepository:
		if addonChartErr != nil {
			chartRes = services.ChartResult{
				Status: status.Failed(addon, helmv1alpha1.ReasonChartFetchFailed, "failed to get desired chart version", addonChartErr),
			}

			break
		}

		// URL change in the HelmClusterAddonRepository may lead to repository type change.
		// If repository type changed from OCI to Helm, we need to remove previously created OCI repository.
		if _, err := r.ociRepositoryService.RemoveOCIRepository(ctx, addon); err != nil {
			chartRes = services.ChartResult{
				Status: status.Failed(addon, helmv1alpha1.ReasonFailed, "Repository change failed", err),
			}
			break
		}

		chartRes = r.chartService.EnsureHelmChart(ctx, addon)
	case utils.InternalOCIRepository:
		if addonChartErr != nil {
			repoRes = services.OCIRepoResult{
				Status: status.Failed(addon, helmv1alpha1.ReasonFailed, "failed to get desired chart version", err),
			}

			break
		}

		if _, err := r.chartService.CleanupHelmChart(ctx, addon); err != nil {
			chartRes = services.ChartResult{
				Status: status.Failed(addon, helmv1alpha1.ReasonFailed, "Repository change failed", err),
			}
			break
		}

		repoRes = r.ociRepositoryService.EnsureInternalOCIRepository(ctx, addon, repo)
	default:
		return reconcile.Result{}, r.statusManager.Update(ctx, addon, status.NoopStatusMutator, status.NoopStatusMapper, services.ReleaseResult{Status: status.Failed(
			addon,
			helmv1alpha1.ReasonFailed,
			fmt.Sprintf("Unsupported repository type: %s", repoType),
			fmt.Errorf("unsupported repository type: %s", repoType),
		)})
	}

	if chartRes.HasArtifact() || repoRes.HasArtifact() {
		var artifactRevision string
		switch repoType {
		case utils.InternalHelmRepository:
			if chartRes.Artifact != nil {
				artifactRevision = chartRes.Artifact.Revision
			}
		case utils.InternalOCIRepository:
			if repoRes.Artifact != nil {
				artifactRevision = repoRes.Artifact.Revision
			}
		}

		releaseRes = r.releaseService.EnsureHelmRelease(ctx, addon, repoType, artifactRevision)
	}

	if err := r.reconcileForceAnnotation(ctx, req); err != nil {
		return reconcile.Result{}, fmt.Errorf("failed to reconcile force annotation: %w", err)
	}

	if err := r.statusManager.Update(
		ctx,
		addon,
		setStatusAttrs(repoType, chartRes, repoRes, releaseRes),
		status.NoopStatusMapper,
		chartRes,
		repoRes,
		releaseRes,
	); client.IgnoreNotFound(err) != nil {
		return reconcile.Result{}, fmt.Errorf("failed to update status: %w", err)
	}

	return reconcile.Result{}, nil
}

func (r *Reconciler) reconcileDelete(ctx context.Context, addon *helmv1alpha1.HelmClusterAddon) (reconcile.Result, error) {
	logger := log.FromContext(ctx)

	if !controllerutil.ContainsFinalizer(addon, helmv1alpha1.FinalizerName) {
		return reconcile.Result{}, nil
	}

	// The finalizer must stay until the internal resources are actually gone.
	// A Delete only sets a deletion timestamp; the downstream controllers keep
	// their finalizers until they finish tearing the underlying release/source
	// down. Removing our finalizer earlier would delete the HelmClusterAddon and
	// orphan a HelmRelease that helm-controller never managed to uninstall.
	//
	// The release is uninstalled first; only once it is gone do we remove the
	// chart/repository sources it referenced. Each step waits for the resource to
	// actually disappear and surfaces the blocking resource's readiness on the
	// addon so the reason a deletion stalls is observable.
	release, err := r.releaseService.CleanupHelmRelease(ctx, addon)
	if err != nil {
		return reconcile.Result{}, err
	}
	if release != nil {
		// The addon is a facade over the HelmRelease: a bad spec parameter that
		// blocks helm uninstall is propagated into the release. Keep re-applying
		// the (possibly corrected) addon spec to the still-present release so the
		// uninstall can be fixed via the addon even while it is being deleted.
		if err := r.releaseService.SyncReleaseSpec(ctx, addon, release); err != nil {
			return reconcile.Result{}, err
		}
		return r.awaitInternalResourceDeletion(ctx, addon, "internal release", release)
	}

	chart, err := r.chartService.CleanupHelmChart(ctx, addon)
	if err != nil {
		return reconcile.Result{}, err
	}
	if chart != nil {
		return r.awaitInternalResourceDeletion(ctx, addon, "internal chart", chart)
	}

	ociRepo, err := r.ociRepositoryService.RemoveOCIRepository(ctx, addon)
	if err != nil {
		return reconcile.Result{}, err
	}
	if ociRepo != nil {
		return r.awaitInternalResourceDeletion(ctx, addon, "internal repository", ociRepo)
	}

	// Release the claim only once every downstream resource is gone: releasing it
	// earlier would let another addon start reconciling the same chart while this
	// one's release is still being uninstalled — exactly the collision the claim
	// prevents.
	if err := r.claimService.Release(ctx, addon); err != nil {
		return reconcile.Result{}, fmt.Errorf("releasing chart claim: %w", err)
	}

	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latestAddon := &helmv1alpha1.HelmClusterAddon{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(addon), latestAddon); err != nil {
			return client.IgnoreNotFound(err)
		}

		if controllerutil.RemoveFinalizer(latestAddon, helmv1alpha1.FinalizerName) {
			if err := r.Update(ctx, latestAddon); err != nil {
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

// awaitInternalResourceDeletion surfaces that an internal resource is still being
// deleted on the addon's status (via the shared status manager) and requeues
// without removing the finalizer. The resource name is kept abstract so its
// internal type is not leaked to the user.
func (r *Reconciler) awaitInternalResourceDeletion(ctx context.Context, addon *helmv1alpha1.HelmClusterAddon, name string, resource status.DeletingResource) (reconcile.Result, error) {
	log.FromContext(ctx).Info("Waiting for internal resource to be deleted before removing finalizer", "resource", name)

	if err := r.statusManager.MarkUninstallPending(ctx, addon, name, resource); client.IgnoreNotFound(err) != nil {
		return reconcile.Result{}, fmt.Errorf("updating deletion status: %w", err)
	}

	return reconcile.Result{RequeueAfter: internalResourceDeletionRequeueInterval}, nil
}

func (r *Reconciler) reconcileAddonNamespace(ctx context.Context, addon *helmv1alpha1.HelmClusterAddon) error {
	ns := &corev1.Namespace{}

	err := r.Get(ctx, client.ObjectKey{Name: addon.Spec.Namespace}, ns)
	if err != nil {
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("getting namespace: %w", err)
		}

		ns = &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: addon.Spec.Namespace,
			},
		}

		err = r.Create(ctx, ns)
		if err != nil {
			if apierrors.IsAlreadyExists(err) {
				return nil
			}
			return fmt.Errorf("creating namespace: %w", err)
		}
	}

	return nil
}

func (r *Reconciler) reconcileForceAnnotation(ctx context.Context, req reconcile.Request) error {
	var addon helmv1alpha1.HelmClusterAddon

	if err := r.Get(ctx, req.NamespacedName, &addon); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("getting helm cluster addon: %w", err)
	}

	if addon.Annotations == nil {
		return nil
	}

	patchBase := client.MergeFrom(addon.DeepCopy())

	delete(addon.Annotations, helmv1alpha1.AnnotationForceReconcile)

	if err := r.Patch(ctx, &addon, patchBase); err != nil {
		return fmt.Errorf("removing force reconcile annotation: %w", err)
	}

	return nil
}

func (r *Reconciler) getHelmClusterAddonChart(ctx context.Context, addon *helmv1alpha1.HelmClusterAddon) (*helmv1alpha1.HelmClusterAddonChart, error) {
	addonChartName := utils.GetHelmClusterAddonChartName(addon.Spec.Chart.HelmClusterAddonRepository, addon.Spec.Chart.HelmClusterAddonChartName)
	addonChart := &helmv1alpha1.HelmClusterAddonChart{}

	err := r.Get(ctx, types.NamespacedName{Name: addonChartName}, addonChart)
	if err != nil {
		return nil, fmt.Errorf("getting helm cluster addon chart: %w", err)
	}

	for _, version := range addonChart.Status.Versions {
		if version.Version == addon.Spec.Chart.Version {
			return addonChart, nil
		}
	}

	return nil, fmt.Errorf("helm cluster addon chart does not have version %q", addon.Spec.Chart.Version)
}

func setStatusAttrs(repoType utils.InternalRepositoryType, chartRes services.ChartResult, repoRes services.OCIRepoResult, releaseRes services.ReleaseResult) status.MutatorFunc {
	return func(obj status.ObjectWithConditions, results []status.Provider) (status.ObjectWithConditions, []status.Provider) {
		results = status.DetermineConditions(obj, results...)
		addon := obj.(*helmv1alpha1.HelmClusterAddon)

		latestRelease := releaseRes.History.Latest()

		var updateChart bool

		switch repoType {
		case utils.InternalHelmRepository:
			if chartRes.HasArtifact() && releaseRes.IsReady() && addon.IsChartStatusInfoOutdated() {
				updateChart = true
			}
		case utils.InternalOCIRepository:
			if repoRes.HasArtifact() && releaseRes.IsReady() && addon.IsChartStatusInfoOutdated() {
				updateChart = true
			}
		}

		if updateChart {
			addon.Status.LastAppliedChart = &helmv1alpha1.HelmClusterAddonLastAppliedChartRef{
				HelmClusterAddonChartName:  addon.Spec.Chart.HelmClusterAddonChartName,
				HelmClusterAddonRepository: addon.Spec.Chart.HelmClusterAddonRepository,
				Version:                    addon.Spec.Chart.Version,
			}
		}

		if releaseRes.IsReady() && latestRelease != nil {
			rawValues := []byte(`{}`)
			if addon.Spec.Values != nil {
				rawValues = addon.Spec.Values.Raw
			}

			addonValues, _ := helmchartutil.ReadValues(rawValues)
			if latestRelease.Status == "deployed" && latestRelease.ConfigDigest == chartutil.DigestValues(digest.Canonical, addonValues).String() {
				if addon.Spec.Values == nil {
					addon.Status.LastAppliedValues = nil
				} else {
					addon.Status.LastAppliedValues = addon.Spec.Values.DeepCopy()
				}
			}
		}

		return obj, results
	}
}
