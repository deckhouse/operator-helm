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

	"github.com/opencontainers/go-digest"
	"github.com/werf/3p-fluxcd-pkg/chartutil"
	helmchartutil "helm.sh/helm/v3/pkg/chartutil"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
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

func New(
	client client.Client,
	chartService *services.ChartService,
	ociRepositoryService *services.OCIRepoService,
	releaseService *services.ReleaseService,
	maintenanceService *services.MaintenanceService,
	statusManager *status.Manager,
) *Reconciler {
	return &Reconciler{
		Client:               client,
		chartService:         chartService,
		ociRepositoryService: ociRepositoryService,
		releaseService:       releaseService,
		maintenanceService:   maintenanceService,
		statusManager:        statusManager,
	}
}

type Reconciler struct {
	client.Client

	chartService         *services.ChartService
	ociRepositoryService *services.OCIRepoService
	releaseService       *services.ReleaseService
	maintenanceService   *services.MaintenanceService
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

	if !controllerutil.ContainsFinalizer(addon, helmv1alpha1.FinalizerName) {
		controllerutil.AddFinalizer(addon, helmv1alpha1.FinalizerName)
		if err := r.Update(ctx, addon); err != nil {
			return reconcile.Result{}, fmt.Errorf("adding finalizer: %w", err)
		}
		return reconcile.Result{}, nil
	}

	err := r.statusManager.InitializeConditions(
		ctx, addon,
		helmv1alpha1.ConditionTypeReady,
		helmv1alpha1.ConditionTypeManaged,
		helmv1alpha1.ConditionTypeInstalled,
		helmv1alpha1.ConditionTypeUpdateInstalled,
		helmv1alpha1.ConditionTypeConfigurationApplied,
		helmv1alpha1.ConditionTypePartiallyDegraded,
	)
	if err != nil {
		return reconcile.Result{}, err
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

	switch repoType {
	case utils.InternalHelmRepository:
		// URL change in the HelmClusterAddonRepository may lead to repository type change.
		// If repository type changed from OCI to Helm, we need to remove previously created OCI repository.
		if err := r.ociRepositoryService.RemoveOCIRepository(ctx, addon); err != nil {
			chartRes = services.ChartResult{
				Status: status.Failed(addon, helmv1alpha1.ReasonFailed, "Repository change failed", err),
			}
			break
		}

		chartRes = r.chartService.EnsureHelmChart(ctx, addon)
		if !chartRes.IsPartiallyDegraded() {
			apimeta.SetStatusCondition(&addon.Status.Conditions, metav1.Condition{
				Type:               helmv1alpha1.ConditionTypePartiallyDegraded,
				Status:             metav1.ConditionFalse,
				ObservedGeneration: addon.Generation,
				Reason:             helmv1alpha1.ReasonSuccess,
			})
		}
	case utils.InternalOCIRepository:
		if err := r.chartService.CleanupHelmChart(ctx, addon); err != nil {
			chartRes = services.ChartResult{
				Status: status.Failed(addon, helmv1alpha1.ReasonFailed, "Repository change failed", err),
			}
			break
		}

		repoRes = r.ociRepositoryService.EnsureInternalOCIRepository(ctx, addon, repo)
		if !repoRes.IsPartiallyDegraded() {
			apimeta.SetStatusCondition(&addon.Status.Conditions, metav1.Condition{
				Type:               helmv1alpha1.ConditionTypePartiallyDegraded,
				Status:             metav1.ConditionFalse,
				ObservedGeneration: addon.Generation,
				Reason:             helmv1alpha1.ReasonSuccess,
			})
		}
	default:
		return reconcile.Result{}, r.statusManager.Update(ctx, addon, status.NoopStatusMutator, status.NoopStatusMapper, services.ReleaseResult{Status: status.Failed(
			addon,
			helmv1alpha1.ReasonFailed,
			fmt.Sprintf("Unsupported repository type: %s", repoType),
			fmt.Errorf("unsupported repository type: %s", repoType),
		)})
	}

	if chartRes.HasArtifact() || repoRes.HasArtifact() {
		releaseRes = r.releaseService.EnsureHelmRelease(ctx, addon, repoType)
	}

	if err := r.reconcileForceAnnotation(ctx, req); err != nil {
		return reconcile.Result{}, fmt.Errorf("failed to reconcile force annotation: %w", err)
	}

	if err := r.statusManager.Update(
		ctx,
		addon,
		setStatusAttrs(repoType, chartRes, repoRes, releaseRes),
		mapResourceStatus(),
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

	if err := r.ociRepositoryService.RemoveOCIRepository(ctx, addon); client.IgnoreNotFound(err) != nil {
		return reconcile.Result{}, err
	}

	if err := r.chartService.CleanupHelmChart(ctx, addon); client.IgnoreNotFound(err) != nil {
		return reconcile.Result{}, err
	}

	if err := r.releaseService.CleanupHelmRelease(ctx, addon); client.IgnoreNotFound(err) != nil {
		return reconcile.Result{}, err
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

func setStatusAttrs(repoType utils.InternalRepositoryType, chartRes services.ChartResult, repoRes services.OCIRepoResult, releaseRes services.ReleaseResult) status.MutatorFunc {
	return func(obj status.ObjectWithConditions, results []status.Provider) (status.ObjectWithConditions, []status.Provider) {
		results = status.DetermineConditions(obj, results...)
		addon := obj.(*helmv1alpha1.HelmClusterAddon)

		var updateChart bool

		switch repoType {
		case utils.InternalHelmRepository:
			if chartRes.HasArtifact() && releaseRes.IsReady() {
				if addon.Status.LastAppliedChart == nil || (addon.IsChartStatusInfoOutdated() && chartRes.IsReady()) {
					updateChart = true
				}
			}
		case utils.InternalOCIRepository:
			if repoRes.HasArtifact() && releaseRes.IsReady() {
				if addon.Status.LastAppliedChart == nil || (addon.IsChartStatusInfoOutdated() && repoRes.IsReady()) {
					updateChart = true
				}
			}
		}

		if updateChart {
			addon.Status.LastAppliedChart = &helmv1alpha1.HelmClusterAddonLastAppliedChartRef{
				HelmClusterAddonChartName:  addon.Spec.Chart.HelmClusterAddonChartName,
				HelmClusterAddonRepository: addon.Spec.Chart.HelmClusterAddonRepository,
				Version:                    addon.Spec.Chart.Version,
			}
		}

		latestRelease := releaseRes.History.Latest()
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

func mapResourceStatus() status.MapperFunc {
	return func(conditionType string, status status.Status) status.Status {
		if conditionType == helmv1alpha1.ConditionTypePartiallyDegraded {
			switch status.Status {
			// ConditionTrue means that HelmChartSucceeded, resetting status would exclude it from result.
			case metav1.ConditionTrue:
				status.Status = ""
			//	ConditionFalse means that chart failed, change Status to True, to raise ConditionTypePartiallyDegraded condition.
			case metav1.ConditionFalse:
				status.Status = metav1.ConditionTrue
			}
		}

		return status
	}
}
