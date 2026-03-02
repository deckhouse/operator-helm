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
	"errors"
	"fmt"

	"github.com/deckhouse/operator-helm/pkg/utils"
	helmv2 "github.com/werf/3p-helm-controller/api/v2"
	sourcev1 "github.com/werf/nelm-source-controller/api/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
)

type Reconciler struct {
	Client client.Client
}

func (r *Reconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	logger := log.FromContext(ctx).WithValues("helmclusteraddon", req.Name)

	var release helmv1alpha1.HelmClusterAddon

	if err := r.Client.Get(ctx, req.NamespacedName, &release); err != nil {
		if apierrors.IsNotFound(err) {
			logger.Info("HelmClusterAddon not found, skipping")

			return reconcile.Result{}, nil
		}

		return reconcile.Result{}, fmt.Errorf("getting HelmClusterAddon: %w", err)
	}

	if !release.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, &release)
	}

	if !controllerutil.ContainsFinalizer(&release, FinalizerName) {
		controllerutil.AddFinalizer(&release, FinalizerName)

		if err := r.Client.Update(ctx, &release); err != nil {
			return reconcile.Result{}, fmt.Errorf("adding finalizer: %w", err)
		}

		return reconcile.Result{}, nil
	}

	var repo helmv1alpha1.HelmClusterAddonRepository

	if err := r.Client.Get(ctx, types.NamespacedName{Name: release.Spec.Chart.HelmClusterAddonRepository}, &repo); err != nil {
		// TODO: rework this condition
		if apierrors.IsNotFound(err) {
			return reconcile.Result{RequeueAfter: 0}, r.patchStatusError(ctx, &release, fmt.Errorf("repository not found: %w", err), ReasonMirrorFailed)
		}

		return reconcile.Result{}, fmt.Errorf("getting HelmClusterAddonRepository: %w", err)
	}

	if err := r.reconcileInternalHelmChart(ctx, &release, &repo); err != nil {
		return reconcile.Result{}, r.patchStatusError(ctx, &release, fmt.Errorf("internal helm chart reconcile failed: %w", err), ReasonMirrorFailed)
	}

	return r.reconcileInternalRelease(ctx, &release)
}

func (r *Reconciler) reconcileInternalHelmChart(ctx context.Context, release *helmv1alpha1.HelmClusterAddon, repo *helmv1alpha1.HelmClusterAddonRepository) error {
	logger := log.FromContext(ctx)

	var addonChart helmv1alpha1.HelmClusterAddonChart

	if err := r.Client.Get(
		ctx,
		types.NamespacedName{
			Name: utils.GetHelmClusterAddonChartName(release.Spec.Chart.HelmClusterAddonRepository,
				release.Spec.Chart.HelmClusterAddonRepository),
		},
		&addonChart,
	); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("addon chart not found: %w", err)
		}

		return fmt.Errorf("getting HelmClusterAddonChart: %w", err)
	}

	// TODO: implement logic depending on pulled flag in the HelmClusterAddonChart
	//for _, chartInfo := range addonChart.Status.Versions {
	//	if chartInfo.Pulled && chartInfo.Version == release.Spec.Chart.Version {
	//
	//	}
	//}

	repoType, _ := utils.GetRepositoryType(repo.Spec.URL)

	existing := &sourcev1.HelmChart{
		ObjectMeta: metav1.ObjectMeta{
			Name: utils.GetInternalHelmChartName(
				release.Name,
				release.Spec.Chart.HelmClusterAddonChartName,
				release.Spec.Chart.Version),
			Namespace: TargetNamespace,
		},
	}

	op, err := controllerutil.CreateOrPatch(ctx, r.Client, existing, func() error {
		if existing.Labels == nil {
			existing.Labels = map[string]string{}
		}

		existing.Labels[LabelManagedBy] = LabelManagedByValue
		existing.Labels[LabelSourceName] = release.Name

		existing.Spec.Chart = release.Spec.Chart.HelmClusterAddonChartName
		existing.Spec.Version = release.Spec.Chart.Version

		switch repoType {
		case utils.InternalHelmRepository:
			existing.Spec.SourceRef = sourcev1.LocalHelmChartSourceReference{
				Kind: sourcev1.HelmRepositoryKind,
				Name: release.Spec.Chart.HelmClusterAddonRepository,
			}
		case utils.InternalOCIRepository:
			existing.Spec.SourceRef = sourcev1.LocalHelmChartSourceReference{
				Kind: sourcev1.OCIRepositoryKind,
				Name: release.Spec.Chart.HelmClusterAddonRepository,
			}
		default:
			return fmt.Errorf("invalid repository type: %s", repoType)
		}

		return nil
	})
	if err != nil {
		return fmt.Errorf("cannot create or update internal release: %w", err)
	}

	if op != controllerutil.OperationResultNone {
		logger.Info("Successfully reconciled internal helm chart", "operation", op, "repository", repo.Name, "chart", release.Spec.Chart.HelmClusterAddonChartName)
	}

	return nil
}

func (r *Reconciler) reconcileInternalRelease(ctx context.Context, release *helmv1alpha1.HelmClusterAddon) (reconcile.Result, error) {
	logger := log.FromContext(ctx)

	existing := &helmv2.HelmRelease{
		ObjectMeta: metav1.ObjectMeta{
			Name:      release.Name,
			Namespace: TargetNamespace,
		},
	}

	op, err := controllerutil.CreateOrPatch(ctx, r.Client, existing, func() error {
		if existing.Labels == nil {
			existing.Labels = map[string]string{}
		}

		existing.Labels[LabelManagedBy] = LabelManagedByValue
		existing.Labels[LabelSourceName] = release.Name

		existing.Spec.ReleaseName = release.Name
		existing.Spec.TargetNamespace = release.Spec.Namespace
		existing.Spec.Values = release.Spec.Values

		existing.Spec.DriftDetection = &helmv2.DriftDetection{
			Mode: helmv2.DriftDetectionWarn,
		}

		if release.Spec.Maintanace != "" {
			existing.Spec.DriftDetection.Mode = helmv2.DriftDetectionEnabled
		}

		existing.Spec.ChartRef = &helmv2.CrossNamespaceSourceReference{
			Kind: sourcev1.HelmChartKind,
			Name: utils.GetInternalHelmChartName(
				release.Name,
				release.Spec.Chart.HelmClusterAddonChartName,
				release.Spec.Chart.Version),
			Namespace: TargetNamespace,
		}

		return nil
	})
	if err != nil {
		return reconcile.Result{}, r.patchStatusError(ctx, release, fmt.Errorf("reconcile internal helm release: %w", err), ReasonMirrorFailed)
	}

	if op != controllerutil.OperationResultNone {
		logger.Info("Successfully reconciled internal helm release", "operation", op, "chart", release.Spec.Chart.HelmClusterAddonChartName)
	}

	return r.updateSuccessStatus(ctx, release, existing.Status.Conditions)
}

// ensureResourceDeleted safely deletes an object if it exists.
func (r *Reconciler) ensureResourceDeleted(ctx context.Context, name, namespace string, obj client.Object) error {
	if err := r.Client.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, obj); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}

		return fmt.Errorf("checking existence of obsolete resource: %w", err)
	}

	if err := r.Client.Delete(ctx, obj); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("deleting obsolete resource: %w", err)
	}

	return nil
}

// reconcileDelete handles cleanup when the HelmClusterRepository is being deleted.
func (r *Reconciler) reconcileDelete(ctx context.Context, release *helmv1alpha1.HelmClusterAddon) (reconcile.Result, error) {
	logger := log.FromContext(ctx).WithValues("helmclusteraddon", release.Name)

	if !controllerutil.ContainsFinalizer(release, FinalizerName) {
		return reconcile.Result{}, nil
	}

	if err := r.ensureResourceDeleted(ctx, release.Name, TargetNamespace, &helmv2.HelmRelease{}); err != nil {
		return reconcile.Result{}, r.patchStatusError(ctx, release, fmt.Errorf("deleting internal helm release: %w", err), ReasonCleanupFailed)
	}

	if err := r.ensureResourceDeleted(ctx, utils.GetInternalHelmChartName(release.Spec.Chart.HelmClusterAddonRepository, release.Spec.Chart.HelmClusterAddonChartName, release.Spec.Chart.Version), TargetNamespace, &sourcev1.HelmChart{}); err != nil {
		return reconcile.Result{}, r.patchStatusError(ctx, release, fmt.Errorf("deleting internal helm chart: %w", err), ReasonCleanupFailed)
	}

	controllerutil.RemoveFinalizer(release, FinalizerName)

	if err := r.Client.Update(ctx, release); err != nil {
		return reconcile.Result{}, fmt.Errorf("removing finalizer: %w", err)
	}

	logger.Info("Cleanup complete")

	return reconcile.Result{}, nil
}

// patchStatusError is a helper to safely patch a failure condition onto the cluster resource.
func (r *Reconciler) patchStatusError(ctx context.Context, release *helmv1alpha1.HelmClusterAddon, reconcileErr error, reason string) error {
	base := release.DeepCopy()

	r.setCondition(release, metav1.ConditionFalse, reason, reconcileErr.Error())

	if patchErr := r.Client.Status().Patch(ctx, release, client.MergeFrom(base)); patchErr != nil {
		return errors.Join(reconcileErr, fmt.Errorf("failed to patch status: %w", patchErr))
	}

	return reconcileErr
}

// updateSuccessStatus patches the status of the cluster resource after a successful reconciliation.
func (r *Reconciler) updateSuccessStatus(ctx context.Context, release *helmv1alpha1.HelmClusterAddon, internalConditions []metav1.Condition) (reconcile.Result, error) {
	base := release.DeepCopy()

	release.Status.Conditions = MapInternalStatusToClusterConditions(internalConditions)
	release.Status.ObservedGeneration = release.Generation

	if err := r.Client.Status().Patch(ctx, release, client.MergeFrom(base)); err != nil {
		return reconcile.Result{}, fmt.Errorf("patching internal custom resource status: %w", err)
	}

	return reconcile.Result{}, nil
}

// setCondition is a helper to set a single Ready condition on the cluster resource.
func (r *Reconciler) setCondition(release *helmv1alpha1.HelmClusterAddon, status metav1.ConditionStatus, reason, message string) {
	now := metav1.Now()

	newCond := metav1.Condition{
		Type:               ConditionTypeReady,
		Status:             status,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: now,
		ObservedGeneration: release.Generation,
	}

	for i, c := range release.Status.Conditions {
		if c.Type == ConditionTypeReady {
			// Only update LastTransitionTime if status actually changed.
			if c.Status == status {
				newCond.LastTransitionTime = c.LastTransitionTime
			}

			release.Status.Conditions[i] = newCond

			return
		}
	}

	release.Status.Conditions = append(release.Status.Conditions, newCond)
}
