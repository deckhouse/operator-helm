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
	"time"

	"github.com/deckhouse/operator-helm/pkg/controller/helmclusteraddonchart"
	"github.com/deckhouse/operator-helm/pkg/utils"
	helmv2 "github.com/werf/3p-helm-controller/api/v2"
	sourcev1 "github.com/werf/nelm-source-controller/api/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
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

	var addon helmv1alpha1.HelmClusterAddon

	if err := r.Client.Get(ctx, req.NamespacedName, &addon); err != nil {
		if apierrors.IsNotFound(err) {
			logger.Info("HelmClusterAddon not found, skipping")

			return reconcile.Result{}, nil
		}

		return reconcile.Result{}, fmt.Errorf("getting HelmClusterAddon: %w", err)
	}

	// Initialize conditions
	if meta.FindStatusCondition(addon.Status.Conditions, ConditionTypeReady) == nil {
		return r.initializeConditions(ctx, &addon)
	}

	if !addon.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, &addon)
	}

	if !controllerutil.ContainsFinalizer(&addon, FinalizerName) {
		controllerutil.AddFinalizer(&addon, FinalizerName)

		if err := r.Client.Update(ctx, &addon); err != nil {
			return reconcile.Result{}, fmt.Errorf("adding finalizer: %w", err)
		}

		return reconcile.Result{}, nil
	}

	// Check if maintenance mode is set
	managedCond := meta.FindStatusCondition(addon.Status.Conditions, ConditionTypeManaged)
	if managedCond == nil {
		return reconcile.Result{}, fmt.Errorf("managed condition is not initialized")
	} else if managedCond.Status == metav1.ConditionTrue && addon.Spec.Maintanace != "" {
		return reconcile.Result{}, nil
	}

	var repo helmv1alpha1.HelmClusterAddonRepository

	if err := r.Client.Get(ctx, types.NamespacedName{Name: addon.Spec.Chart.HelmClusterAddonRepository}, &repo); err != nil {
		if apierrors.IsNotFound(err) {
			return reconcile.Result{}, r.patchStatusError(ctx, &addon, fmt.Errorf("repository not found: %w", err), ReasonMirrorFailed)
		}

		return reconcile.Result{}, fmt.Errorf("getting HelmClusterAddonRepository: %w", err)
	}

	if err := r.reconcileInternalHelmChart(ctx, &addon, &repo); err != nil {
		return reconcile.Result{}, r.patchStatusError(ctx, &addon, fmt.Errorf("internal helm chart reconcile failed: %w", err), ReasonMirrorFailed)
	}

	return r.reconcileInternalRelease(ctx, &addon)
}

func (r *Reconciler) reconcileInternalHelmChart(ctx context.Context, addon *helmv1alpha1.HelmClusterAddon, repo *helmv1alpha1.HelmClusterAddonRepository) error {
	logger := log.FromContext(ctx)

	repoType, _ := utils.GetRepositoryType(repo.Spec.URL)

	existing := &sourcev1.HelmChart{
		ObjectMeta: metav1.ObjectMeta{
			Name:      utils.GetInternalHelmChartName(addon.Name),
			Namespace: TargetNamespace,
		},
	}

	op, err := controllerutil.CreateOrPatch(ctx, r.Client, existing, func() error {
		if existing.Labels == nil {
			existing.Labels = map[string]string{}
		}

		existing.Labels[LabelManagedBy] = LabelManagedByValue
		existing.Labels[LabelSourceName] = addon.Name
		existing.Labels[helmclusteraddonchart.LabelSourceName] = utils.GetHelmClusterAddonChartName(
			repo.Name, addon.Spec.Chart.HelmClusterAddonChartName)

		existing.Spec.Chart = addon.Spec.Chart.HelmClusterAddonChartName
		existing.Spec.Version = addon.Spec.Chart.Version

		switch repoType {
		case utils.InternalHelmRepository:
			existing.Spec.SourceRef = sourcev1.LocalHelmChartSourceReference{
				Kind: sourcev1.HelmRepositoryKind,
				Name: addon.Spec.Chart.HelmClusterAddonRepository,
			}
		case utils.InternalOCIRepository:
			existing.Spec.SourceRef = sourcev1.LocalHelmChartSourceReference{
				Kind: sourcev1.OCIRepositoryKind,
				Name: addon.Spec.Chart.HelmClusterAddonRepository,
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
		logger.Info("Successfully reconciled internal helm chart", "operation", op, "repository", repo.Name, "chart", addon.Spec.Chart.HelmClusterAddonChartName)
	}

	return nil
}

func (r *Reconciler) reconcileInternalRelease(ctx context.Context, addon *helmv1alpha1.HelmClusterAddon) (reconcile.Result, error) {
	logger := log.FromContext(ctx)

	var addonChart helmv1alpha1.HelmClusterAddonChart

	if err := r.Client.Get(
		ctx,
		types.NamespacedName{
			Name: utils.GetHelmClusterAddonChartName(addon.Spec.Chart.HelmClusterAddonRepository,
				addon.Spec.Chart.HelmClusterAddonRepository),
		},
		&addonChart,
	); err != nil {
		if apierrors.IsNotFound(err) {
			return reconcile.Result{}, r.patchStatusError(ctx, addon, fmt.Errorf("addon chart not found: %w", err), ReasonMirrorFailed)
		}

		return reconcile.Result{}, r.patchStatusError(ctx, addon, fmt.Errorf("getting HelmClusterAddonChart: %w", err), ReasonMirrorFailed)
	}

	var chartPulled bool
	for _, chartInfo := range addonChart.Status.Versions {
		if addon.Spec.Chart.Version == chartInfo.Version {
			chartPulled = chartInfo.Pulled
		}
	}

	if !chartPulled {
		// TODO: magic number
		return reconcile.Result{RequeueAfter: 10 * time.Second}, nil
	}

	existing := &helmv2.HelmRelease{
		ObjectMeta: metav1.ObjectMeta{
			Name:      utils.GetInternalHelmReleaseName(addon.Name),
			Namespace: TargetNamespace,
		},
	}

	op, err := controllerutil.CreateOrPatch(ctx, r.Client, existing, func() error {
		if existing.Labels == nil {
			existing.Labels = map[string]string{}
		}

		existing.Labels[LabelManagedBy] = LabelManagedByValue
		existing.Labels[LabelSourceName] = addon.Name

		existing.Spec.ReleaseName = addon.Name
		existing.Spec.TargetNamespace = addon.Spec.Namespace
		existing.Spec.Values = addon.Spec.Values

		existing.Spec.Suspend = false

		if addon.Spec.Maintanace != "" {
			existing.Spec.Suspend = true
		}

		existing.Spec.ChartRef = &helmv2.CrossNamespaceSourceReference{
			Kind:      sourcev1.HelmChartKind,
			Name:      utils.GetInternalHelmChartName(addon.Name),
			Namespace: TargetNamespace,
		}

		return nil
	})
	if err != nil {
		return reconcile.Result{}, r.patchStatusError(ctx, addon, fmt.Errorf("reconcile internal helm release: %w", err), ReasonMirrorFailed)
	}

	if op != controllerutil.OperationResultNone {
		logger.Info("Successfully reconciled internal helm release", "operation", op, "chart", addon.Spec.Chart.HelmClusterAddonChartName)
	}

	return r.updateSuccessStatus(ctx, addon, existing.Status.Conditions)
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
func (r *Reconciler) reconcileDelete(ctx context.Context, addon *helmv1alpha1.HelmClusterAddon) (reconcile.Result, error) {
	logger := log.FromContext(ctx).WithValues("helmclusteraddon", addon.Name)

	if !controllerutil.ContainsFinalizer(addon, FinalizerName) {
		return reconcile.Result{}, nil
	}

	if err := r.ensureResourceDeleted(ctx, utils.GetInternalHelmReleaseName(addon.Name), TargetNamespace, &helmv2.HelmRelease{}); err != nil {
		return reconcile.Result{}, r.patchStatusError(ctx, addon, fmt.Errorf("deleting internal helm release: %w", err), ReasonCleanupFailed)
	}

	if err := r.ensureResourceDeleted(ctx, utils.GetInternalHelmChartName(addon.Name), TargetNamespace, &sourcev1.HelmChart{}); err != nil {
		return reconcile.Result{}, r.patchStatusError(ctx, addon, fmt.Errorf("deleting internal helm chart: %w", err), ReasonCleanupFailed)
	}

	controllerutil.RemoveFinalizer(addon, FinalizerName)

	if err := r.Client.Update(ctx, addon); err != nil {
		return reconcile.Result{}, fmt.Errorf("removing finalizer: %w", err)
	}

	logger.Info("Cleanup complete")

	return reconcile.Result{}, nil
}

func (r *Reconciler) initializeConditions(ctx context.Context, addon *helmv1alpha1.HelmClusterAddon) (reconcile.Result, error) {
	conditionTypes := []string{
		ConditionTypeReady,
		ConditionTypeManaged,
		ConditionTypeConfigurationApplied,
		ConditionTypeInstalled,
		ConditionTypeUpdateInstalled,
		ConditionTypeInstalled,
	}

	for _, t := range conditionTypes {
		if meta.FindStatusCondition(addon.Status.Conditions, t) == nil {
			meta.SetStatusCondition(&addon.Status.Conditions, metav1.Condition{
				Type:   t,
				Status: metav1.ConditionUnknown,
				Reason: ReasonInitializing,
			})
		}
	}

	if err := r.Client.Status().Update(ctx, addon); err != nil {
		return reconcile.Result{}, fmt.Errorf("updating HelmClusterAddon status conditions: %w", err)
	}

	return reconcile.Result{}, nil
}

// patchStatusError is a helper to safely patch a failure condition onto the cluster resource.
func (r *Reconciler) patchStatusError(ctx context.Context, addon *helmv1alpha1.HelmClusterAddon, reconcileErr error, reason string) error {
	base := addon.DeepCopy()

	r.setCondition(addon, ConditionTypeReady, metav1.ConditionFalse, reason, reconcileErr.Error())

	if patchErr := r.Client.Status().Patch(ctx, addon, client.MergeFrom(base)); patchErr != nil {
		return errors.Join(reconcileErr, fmt.Errorf("failed to patch status: %w", patchErr))
	}

	return reconcileErr
}

// updateSuccessStatus patches the status of the cluster resource after a successful reconciliation.
func (r *Reconciler) updateSuccessStatus(ctx context.Context, addon *helmv1alpha1.HelmClusterAddon, internalConditions []metav1.Condition) (reconcile.Result, error) {
	base := addon.DeepCopy()

	internalReadyCond := meta.FindStatusCondition(internalConditions, ConditionTypeReady)
	if internalReadyCond != nil {
		r.setCondition(addon, ConditionTypeReady, internalReadyCond.Status, internalReadyCond.Reason, internalReadyCond.Message)
	}

	addon.Status.ObservedGeneration = addon.Generation

	if addon.Spec.Maintanace == "" {
		r.setCondition(addon, ConditionTypeManaged, metav1.ConditionTrue, ReasonManagedModeActivated, "")
	} else {
		r.setCondition(addon, ConditionTypeManaged, metav1.ConditionFalse, ReasonUnmanagedModeActivated, "")
	}

	if err := r.Client.Status().Patch(ctx, addon, client.MergeFrom(base)); err != nil {
		return reconcile.Result{}, fmt.Errorf("updating HelmClusterAddon status on success: %w", err)
	}

	return reconcile.Result{}, nil
}

// setCondition is a helper to set a single Ready condition on the cluster resource.
func (r *Reconciler) setCondition(addon *helmv1alpha1.HelmClusterAddon, conditionType string, status metav1.ConditionStatus, reason, message string) {
	now := metav1.Now()

	newCond := metav1.Condition{
		Type:               conditionType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: now,
		ObservedGeneration: addon.Generation,
	}

	for i, c := range addon.Status.Conditions {
		if c.Type == conditionType {
			if c.Status == status {
				newCond.LastTransitionTime = c.LastTransitionTime
			}

			addon.Status.Conditions[i] = newCond

			return
		}
	}

	addon.Status.Conditions = append(addon.Status.Conditions, newCond)
}
