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
	"reflect"

	"github.com/opencontainers/go-digest"
	"github.com/werf/3p-fluxcd-pkg/chartutil"
	helmv2 "github.com/werf/3p-helm-controller/api/v2"
	sourcev1 "github.com/werf/nelm-source-controller/api/v1"
	helmchartutil "helm.sh/helm/v3/pkg/chartutil"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
	"github.com/deckhouse/operator-helm/internal/controller/helmclusteraddonchart"
	"github.com/deckhouse/operator-helm/internal/utils"
)

type reconciler struct {
	client client.Client
}

type addonState struct {
	addon                  *helmv1alpha1.HelmClusterAddon
	base                   *helmv1alpha1.HelmClusterAddon
	addonRepository        *helmv1alpha1.HelmClusterAddonRepository
	internalRepositoryType utils.InternalRepositoryType
	internalHelmRelease    *helmv2.HelmRelease
	internalHelmChart      *sourcev1.HelmChart
}

func (r *reconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	logger := log.FromContext(ctx).WithValues("helmclusteraddon", req.Name)
	ctx = log.IntoContext(ctx, logger)

	state := &addonState{addon: &helmv1alpha1.HelmClusterAddon{}}

	if err := r.client.Get(ctx, req.NamespacedName, state.addon); err != nil {
		if apierrors.IsNotFound(err) {
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, fmt.Errorf("getting HelmClusterAddon: %w", err)
	}

	state.base = state.addon.DeepCopy()

	if done, err := r.phaseDelete(ctx, state); done || err != nil {
		return reconcile.Result{}, err
	}
	if done, result, err := r.phaseBootstrap(ctx, state); done || err != nil {
		return result, err
	}
	if done, err := r.phaseMaintenance(ctx, state); done || err != nil {
		return reconcile.Result{}, err
	}
	return r.phaseSync(ctx, state)
}

func (r *reconciler) phaseDelete(ctx context.Context, state *addonState) (bool, error) {
	logger := log.FromContext(ctx)

	if state.addon.DeletionTimestamp.IsZero() {
		return false, nil
	}

	if !controllerutil.ContainsFinalizer(state.addon, FinalizerName) {
		return true, nil
	}

	if err := r.ensureResourceDeleted(ctx, utils.GetInternalHelmReleaseName(state.addon.Name), TargetNamespace, &helmv2.HelmRelease{}); err != nil {
		return true, fmt.Errorf("deleting internal helm release: %w", err)
	}

	if err := r.ensureResourceDeleted(ctx, utils.GetInternalHelmChartName(state.addon.Name), TargetNamespace, &sourcev1.HelmChart{}); err != nil {
		return true, fmt.Errorf("deleting internal helm chart: %w", err)
	}

	controllerutil.RemoveFinalizer(state.addon, FinalizerName)

	if err := r.client.Update(ctx, state.addon); err != nil {
		return true, fmt.Errorf("removing finalizer: %w", err)
	}

	logger.Info("Cleanup complete")

	return true, nil
}

func (r *reconciler) phaseBootstrap(ctx context.Context, state *addonState) (bool, reconcile.Result, error) {
	conditionsInitialized := apimeta.FindStatusCondition(state.addon.Status.Conditions, ConditionTypeReady) != nil
	finalizerPresent := controllerutil.ContainsFinalizer(state.addon, FinalizerName)

	if conditionsInitialized && finalizerPresent {
		return false, reconcile.Result{}, nil
	}

	if !conditionsInitialized {
		for _, t := range []string{ConditionTypeReady, ConditionTypeManaged} {
			if apimeta.FindStatusCondition(state.addon.Status.Conditions, t) == nil {
				apimeta.SetStatusCondition(&state.addon.Status.Conditions, metav1.Condition{
					Type:   t,
					Status: metav1.ConditionUnknown,
					Reason: ReasonInitializing,
				})
			}
		}

		if err := r.client.Status().Update(ctx, state.addon); err != nil {
			return true, reconcile.Result{}, fmt.Errorf("updating helm cluster addon status conditions: %w", err)
		}

		return true, reconcile.Result{RequeueAfter: ReconcileRetryInterval}, nil
	}

	controllerutil.AddFinalizer(state.addon, FinalizerName)

	if err := r.client.Update(ctx, state.addon); err != nil {
		return true, reconcile.Result{}, fmt.Errorf("adding finalizer to helm cluster addon: %w", err)
	}

	return true, reconcile.Result{RequeueAfter: ReconcileRetryInterval}, nil
}

func (r *reconciler) ensureInternalHelmReleaseSuspended(ctx context.Context, addonName string) (*helmv2.HelmRelease, error) {
	helmRelease := &helmv2.HelmRelease{}
	if err := r.client.Get(ctx, types.NamespacedName{
		Name:      utils.GetInternalHelmReleaseName(addonName),
		Namespace: TargetNamespace,
	}, helmRelease); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}

		return nil, fmt.Errorf("getting internal helm release: %w", err)
	}

	if helmRelease.Spec.Suspend {
		return helmRelease, nil
	}

	base := helmRelease.DeepCopy()

	helmRelease.Spec.Suspend = true

	if err := r.client.Patch(ctx, helmRelease, client.MergeFrom(base)); err != nil {
		return nil, fmt.Errorf("suspending internal helm release: %w", err)
	}

	return helmRelease, nil
}

func (r *reconciler) phaseMaintenance(ctx context.Context, state *addonState) (bool, error) {
	managedCond := apimeta.FindStatusCondition(state.addon.Status.Conditions, ConditionTypeManaged)
	if managedCond == nil {
		return true, fmt.Errorf("reading managed condition: not initialized")
	}

	if state.addon.Spec.Maintenance != string(helmv1alpha1.NoResourceReconciliation) {
		return false, nil
	}

	helmRelease, err := r.ensureInternalHelmReleaseSuspended(ctx, state.addon.Name)
	if err != nil {
		return true, err
	}

	if helmRelease != nil && !helmRelease.Spec.Suspend {
		return true, fmt.Errorf("internal helm release suspend not confirmed")
	}

	if managedCond.Status == metav1.ConditionFalse {
		return true, nil
	}

	apimeta.SetStatusCondition(&state.addon.Status.Conditions, metav1.Condition{
		Type:               ConditionTypeManaged,
		Status:             metav1.ConditionFalse,
		Reason:             ReasonUnmanagedModeActivated,
		ObservedGeneration: state.addon.Generation,
	})

	apimeta.SetStatusCondition(&state.addon.Status.Conditions, metav1.Condition{
		Type:               ConditionTypeReady,
		Status:             metav1.ConditionFalse,
		Reason:             ReasonUnmanagedModeActivated,
		ObservedGeneration: state.addon.Generation,
	})

	state.addon.Status.ObservedGeneration = state.addon.Generation

	if err := r.client.Status().Patch(ctx, state.addon, client.MergeFrom(state.base)); err != nil {
		return true, fmt.Errorf("patching status for maintenance mode: %w", err)
	}

	return true, nil
}

func (r *reconciler) phaseSync(ctx context.Context, state *addonState) (reconcile.Result, error) {
	requeue, err := r.setStatusToProcessing(ctx, state)
	if err != nil {
		return reconcile.Result{}, err
	}
	if requeue {
		return reconcile.Result{RequeueAfter: ReconcileRetryInterval}, nil
	}

	if err := r.getRepository(ctx, state); err != nil {
		return r.updateStatus(ctx, state, err)
	}

	switch state.internalRepositoryType {
	case utils.InternalHelmRepository:
		if err := r.reconcileInternalHelmChart(ctx, state); err != nil {
			return r.updateStatus(ctx, state, err)
		}
	case utils.InternalOCIRepository:
		// TODO: add internal OCI repository reconcile here.
	}

	shouldReconcileRelease := state.internalRepositoryType == utils.InternalOCIRepository ||
		(state.internalHelmChart != nil && state.internalHelmChart.Status.Artifact != nil)

	if shouldReconcileRelease {
		if err := r.reconcileInternalHelmRelease(ctx, state); err != nil {
			return r.updateStatus(ctx, state, err)
		}
	}

	return r.updateStatus(ctx, state, nil)
}

func (r *reconciler) getRepository(ctx context.Context, state *addonState) error {
	state.addonRepository = &helmv1alpha1.HelmClusterAddonRepository{}

	if err := r.client.Get(ctx, types.NamespacedName{Name: state.addon.Spec.Chart.HelmClusterAddonRepository}, state.addonRepository); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("helm cluster addon repository not found: %w", err)
		}
		return fmt.Errorf("getting helm cluster addon repository: %w", err)
	}

	var err error

	state.internalRepositoryType, err = utils.GetRepositoryType(state.addonRepository.Spec.URL)
	if err != nil {
		return fmt.Errorf("getting helm cluster addon repository type: %w", err)
	}

	return nil
}

func (r *reconciler) setStatusToProcessing(ctx context.Context, state *addonState) (bool, error) {
	readyCond := apimeta.FindStatusCondition(state.addon.Status.Conditions, ConditionTypeReady)

	if state.addon.Generation == state.addon.Status.ObservedGeneration ||
		(readyCond != nil && readyCond.Status == metav1.ConditionFalse && readyCond.Reason == ReasonProcessing) {
		return false, nil
	}

	apimeta.SetStatusCondition(&state.addon.Status.Conditions, metav1.Condition{
		Type:    ConditionTypeReady,
		Status:  metav1.ConditionFalse,
		Reason:  ReasonProcessing,
		Message: "",
	})

	if err := r.client.Status().Patch(ctx, state.addon, client.MergeFrom(state.base)); err != nil {
		return false, fmt.Errorf("updating helm cluster addon status: %w", err)
	}

	return true, nil
}

func (r *reconciler) reconcileInternalHelmChart(ctx context.Context, state *addonState) error {
	logger := log.FromContext(ctx)

	repoType, err := utils.GetRepositoryType(state.addonRepository.Spec.URL)
	if err != nil {
		return fmt.Errorf("getting repository type: %w", err)
	}

	state.internalHelmChart = &sourcev1.HelmChart{
		ObjectMeta: metav1.ObjectMeta{
			Name:      utils.GetInternalHelmChartName(state.addon.Name),
			Namespace: TargetNamespace,
		},
	}

	op, err := controllerutil.CreateOrPatch(ctx, r.client, state.internalHelmChart, func() error {
		if state.internalHelmChart.Labels == nil {
			state.internalHelmChart.Labels = map[string]string{}
		}

		state.internalHelmChart.Labels[LabelManagedBy] = LabelManagedByValue
		state.internalHelmChart.Labels[LabelSourceName] = state.addon.Name
		state.internalHelmChart.Labels[helmclusteraddonchart.LabelSourceName] = utils.GetHelmClusterAddonChartName(
			state.addon.Spec.Chart.HelmClusterAddonRepository, state.addon.Spec.Chart.HelmClusterAddonChartName)

		state.internalHelmChart.Spec.Chart = state.addon.Spec.Chart.HelmClusterAddonChartName
		state.internalHelmChart.Spec.Version = state.addon.Spec.Chart.Version

		switch repoType {
		case utils.InternalHelmRepository:
			state.internalHelmChart.Spec.SourceRef = sourcev1.LocalHelmChartSourceReference{
				Kind: sourcev1.HelmRepositoryKind,
				Name: state.addon.Spec.Chart.HelmClusterAddonRepository,
			}
		case utils.InternalOCIRepository:
			state.internalHelmChart.Spec.SourceRef = sourcev1.LocalHelmChartSourceReference{
				Kind: sourcev1.OCIRepositoryKind,
				Name: state.addon.Spec.Chart.HelmClusterAddonRepository,
			}
		default:
			return fmt.Errorf("invalid repository type: %s", repoType)
		}

		return nil
	})
	if err != nil {
		return fmt.Errorf("creating or updating internal helm chart: %w", err)
	}

	if _, ok := utils.IsConditionObserved(state.internalHelmChart.GetConditions(), ConditionTypeReady, state.internalHelmChart.Generation); ok {
		logger.Info("Successfully reconciled internal helm chart", "operation", op, "repository", state.addon.Spec.Chart.HelmClusterAddonRepository, "chart", state.addon.Spec.Chart.HelmClusterAddonChartName)
	}

	return nil
}

func (r *reconciler) reconcileInternalHelmRelease(ctx context.Context, state *addonState) error {
	logger := log.FromContext(ctx)

	state.internalHelmRelease = &helmv2.HelmRelease{
		ObjectMeta: metav1.ObjectMeta{
			Name:      utils.GetInternalHelmReleaseName(state.addon.Name),
			Namespace: TargetNamespace,
		},
	}

	op, err := controllerutil.CreateOrPatch(ctx, r.client, state.internalHelmRelease, func() error {
		if state.internalHelmRelease.Labels == nil {
			state.internalHelmRelease.Labels = map[string]string{}
		}

		state.internalHelmRelease.Labels[LabelManagedBy] = LabelManagedByValue
		state.internalHelmRelease.Labels[LabelSourceName] = state.addon.Name

		state.internalHelmRelease.Spec.ReleaseName = state.addon.Name
		state.internalHelmRelease.Spec.TargetNamespace = state.addon.Spec.Namespace
		state.internalHelmRelease.Spec.Values = state.addon.Spec.Values

		state.internalHelmRelease.Spec.Suspend = false

		switch state.internalRepositoryType {
		case utils.InternalHelmRepository:
			state.internalHelmRelease.Spec.ChartRef = &helmv2.CrossNamespaceSourceReference{
				Kind:      sourcev1.HelmChartKind,
				Name:      utils.GetInternalHelmChartName(state.addon.Name),
				Namespace: TargetNamespace,
			}
		case utils.InternalOCIRepository:
			state.internalHelmRelease.Spec.ChartRef = &helmv2.CrossNamespaceSourceReference{
				Kind:      sourcev1.OCIRepositoryKind,
				Name:      state.addon.Spec.Chart.HelmClusterAddonRepository,
				Namespace: TargetNamespace,
			}
		default:
			return fmt.Errorf("invalid repository type: %s", state.internalRepositoryType)
		}

		return nil
	})
	if err != nil {
		return fmt.Errorf("reconciling internal helm release: %w", err)
	}

	if _, ok := utils.IsConditionObserved(state.internalHelmRelease.GetConditions(), ConditionTypeReady, state.internalHelmRelease.Generation); ok {
		logger.Info("Successfully reconciled internal helm release", "operation", op, "chart", state.addon.Spec.Chart.HelmClusterAddonChartName)
	}

	return nil
}

func (r *reconciler) updateStatus(ctx context.Context, state *addonState, syncErr error) (reconcile.Result, error) {
	logger := log.FromContext(ctx)

	if syncErr != nil {
		logger.Error(syncErr, "sync phase failed")

		return r.updateStatusOnSyncError(ctx, state, syncErr)
	}

	if state.addon.Spec.Maintenance != string(helmv1alpha1.NoResourceReconciliation) {
		apimeta.SetStatusCondition(&state.addon.Status.Conditions, metav1.Condition{
			Type:               ConditionTypeManaged,
			Status:             metav1.ConditionTrue,
			Reason:             ReasonManagedModeActivated,
			ObservedGeneration: state.addon.Generation,
		})
	}

	var helmChartReadyCond *metav1.Condition

	if state.internalHelmChart != nil {
		helmChartReadyCond = apimeta.FindStatusCondition(state.internalHelmChart.Status.Conditions, ConditionTypeReady)
		if helmChartReadyCond == nil || helmChartReadyCond.ObservedGeneration != state.internalHelmChart.Generation {
			logger.Info("Internal helm chart is not observed yet")
			return reconcile.Result{RequeueAfter: ReconcileRetryInterval}, nil
		}

		if state.internalHelmChart.Status.Artifact == nil {
			logger.Info("Internal helm chart has no artifact")
			return r.updateStatusChartNotReady(ctx, state, helmChartReadyCond)
		}
	}

	if state.internalHelmRelease == nil {
		logger.Info("Internal helm release is not created yet")
		return reconcile.Result{RequeueAfter: ReconcileRetryInterval}, nil
	}

	helmReleaseReadyCond := apimeta.FindStatusCondition(state.internalHelmRelease.Status.Conditions, ConditionTypeReady)
	if helmReleaseReadyCond == nil || helmReleaseReadyCond.ObservedGeneration != state.internalHelmRelease.Generation {
		logger.Info("Internal helm release is observed yet")
		return reconcile.Result{RequeueAfter: ReconcileRetryInterval}, nil
	}

	apimeta.SetStatusCondition(&state.addon.Status.Conditions, metav1.Condition{
		Type:               ConditionTypeReady,
		Status:             helmReleaseReadyCond.Status,
		Reason:             helmReleaseReadyCond.Reason,
		ObservedGeneration: state.addon.Generation,
		Message:            helmReleaseReadyCond.Message,
	})

	if helmReleaseReadyCond.Status == metav1.ConditionTrue {
		if helmChartReadyCond != nil && helmChartReadyCond.Status != metav1.ConditionTrue {
			apimeta.SetStatusCondition(&state.addon.Status.Conditions, metav1.Condition{
				Type:               ConditionTypePartiallyDegraded,
				Status:             metav1.ConditionTrue,
				Reason:             helmChartReadyCond.Reason,
				ObservedGeneration: state.addon.Generation,
				Message:            "",
			})
		}
		r.updateStatusHelmReleaseReady(ctx, state)
	} else {
		apimeta.RemoveStatusCondition(&state.addon.Status.Conditions, ConditionTypePartiallyDegraded)
		r.updateStatusHelmReleaseFailed(state, helmReleaseReadyCond)
	}

	state.addon.Status.ObservedGeneration = state.addon.Generation

	if err := r.client.Status().Patch(ctx, state.addon, client.MergeFrom(state.base)); err != nil {
		return reconcile.Result{}, fmt.Errorf("updating helm cluster addon status: %w", err)
	}

	return reconcile.Result{}, nil
}

func (r *reconciler) updateStatusHelmReleaseReady(_ context.Context, state *addonState) {
	firstInstallCompleted := apimeta.IsStatusConditionPresentAndEqual(state.addon.Status.Conditions, ConditionTypeInstalled, metav1.ConditionTrue)

	if !firstInstallCompleted {
		apimeta.SetStatusCondition(&state.addon.Status.Conditions, metav1.Condition{
			Type:               ConditionTypeInstalled,
			Status:             metav1.ConditionTrue,
			Reason:             ReasonInstallSucceeded,
			ObservedGeneration: state.addon.Generation,
			Message:            "",
		})

		apimeta.RemoveStatusCondition(&state.addon.Status.Conditions, ConditionTypeConfigurationApplied)
		apimeta.RemoveStatusCondition(&state.addon.Status.Conditions, ConditionTypeUpdateInstalled)

		if state.internalHelmChart == nil ||
			(apimeta.IsStatusConditionPresentAndEqual(state.internalHelmChart.Status.Conditions, ConditionTypeReady, metav1.ConditionTrue) &&
				state.internalHelmChart.Generation == state.internalHelmChart.Status.ObservedGeneration) {
			state.addon.Status.LastAppliedChart = &helmv1alpha1.HelmClusterAddonLastAppliedChartRef{
				HelmClusterAddonChartName:  state.addon.Spec.Chart.HelmClusterAddonChartName,
				HelmClusterAddonRepository: state.addon.Spec.Chart.HelmClusterAddonRepository,
				Version:                    state.addon.Spec.Chart.Version,
			}
		}

		state.addon.Status.LastAppliedValues = state.addon.Spec.Values
	} else {
		lastAppliedChartUpdateRequired := isLastAppliedChartUpdateRequired(state.addon, state.internalHelmChart, state.internalHelmRelease)

		if lastAppliedChartUpdateRequired {
			apimeta.SetStatusCondition(&state.addon.Status.Conditions, metav1.Condition{
				Type:               ConditionTypeUpdateInstalled,
				Status:             metav1.ConditionTrue,
				Reason:             ReasonUpdateSucceeded,
				ObservedGeneration: state.addon.Generation,
				Message:            "",
			})
			state.addon.Status.LastAppliedChart = &helmv1alpha1.HelmClusterAddonLastAppliedChartRef{
				HelmClusterAddonChartName:  state.addon.Spec.Chart.HelmClusterAddonChartName,
				HelmClusterAddonRepository: state.addon.Spec.Chart.HelmClusterAddonRepository,
				Version:                    state.addon.Spec.Chart.Version,
			}

			return
		}

		// Prevent from unnecessary status update if values already match.
		if reflect.DeepEqual(state.addon.Status.LastAppliedValues, state.addon.Spec.Values) {
			return
		}

		if state.addon.Spec.Values != nil {
			addonValues, _ := helmchartutil.ReadValues(state.addon.Spec.Values.Raw)

			latestRelease := state.internalHelmRelease.Status.History.Latest()
			if latestRelease != nil && latestRelease.Status == InternalHelmReleaseDeployed &&
				latestRelease.ConfigDigest == chartutil.DigestValues(digest.Canonical, addonValues).String() {
				apimeta.SetStatusCondition(&state.addon.Status.Conditions, metav1.Condition{
					Type:               ConditionTypeConfigurationApplied,
					Status:             metav1.ConditionTrue,
					Reason:             ReasonUpdateSucceeded,
					ObservedGeneration: state.addon.Generation,
					Message:            "Applied configuration with values digest " + latestRelease.ConfigDigest,
				})
				state.addon.Status.LastAppliedValues = state.addon.Spec.Values
			}
		} else {
			apimeta.SetStatusCondition(&state.addon.Status.Conditions, metav1.Condition{
				Type:               ConditionTypeConfigurationApplied,
				Status:             metav1.ConditionTrue,
				Reason:             ReasonUpdateSucceeded,
				ObservedGeneration: state.addon.Generation,
				Message:            "",
			})
			state.addon.Status.LastAppliedValues = nil
		}
	}
}

func (r *reconciler) updateStatusHelmReleaseFailed(state *addonState, helmReleaseReadyCond *metav1.Condition) {
	firstInstallCompleted := apimeta.IsStatusConditionPresentAndEqual(state.addon.Status.Conditions, ConditionTypeInstalled, metav1.ConditionTrue)

	if !firstInstallCompleted {
		apimeta.SetStatusCondition(&state.addon.Status.Conditions, metav1.Condition{
			Type:               ConditionTypeInstalled,
			Status:             metav1.ConditionFalse,
			Reason:             helmReleaseReadyCond.Reason,
			ObservedGeneration: state.addon.Generation,
			Message:            helmReleaseReadyCond.Message,
		})

		apimeta.RemoveStatusCondition(&state.addon.Status.Conditions, ConditionTypeConfigurationApplied)
		apimeta.RemoveStatusCondition(&state.addon.Status.Conditions, ConditionTypeUpdateInstalled)
	} else {
		if !reflect.DeepEqual(state.addon.Status.LastAppliedValues, state.addon.Spec.Values) {
			apimeta.SetStatusCondition(&state.addon.Status.Conditions, metav1.Condition{
				Type:               ConditionTypeConfigurationApplied,
				Status:             metav1.ConditionFalse,
				Reason:             helmReleaseReadyCond.Reason,
				ObservedGeneration: state.addon.Generation,
				Message:            helmReleaseReadyCond.Message,
			})
		} else {
			apimeta.SetStatusCondition(&state.addon.Status.Conditions, metav1.Condition{
				Type:               ConditionTypeUpdateInstalled,
				Status:             metav1.ConditionFalse,
				Reason:             helmReleaseReadyCond.Reason,
				ObservedGeneration: state.addon.Generation,
				Message:            helmReleaseReadyCond.Message,
			})
		}
	}
}

func (r *reconciler) updateStatusOnSyncError(ctx context.Context, state *addonState, syncErr error) (reconcile.Result, error) {
	apimeta.SetStatusCondition(&state.addon.Status.Conditions, metav1.Condition{
		Type:               ConditionTypeReady,
		Status:             metav1.ConditionFalse,
		Reason:             ReasonReconcileFailed,
		ObservedGeneration: state.addon.Generation,
		Message:            syncErr.Error(),
	})

	state.addon.Status.ObservedGeneration = state.addon.Generation

	if err := r.client.Status().Patch(ctx, state.addon, client.MergeFrom(state.base)); err != nil {
		return reconcile.Result{}, fmt.Errorf("patching status on error: %w", err)
	}

	return reconcile.Result{RequeueAfter: ReconcileRetryInterval}, nil
}

func (r *reconciler) updateStatusChartNotReady(ctx context.Context, state *addonState, helmChartReadyCond *metav1.Condition) (reconcile.Result, error) {
	apimeta.SetStatusCondition(&state.addon.Status.Conditions, metav1.Condition{
		Type:               ConditionTypeReady,
		Status:             metav1.ConditionFalse,
		Reason:             helmChartReadyCond.Reason,
		ObservedGeneration: state.addon.Generation,
		Message:            helmChartReadyCond.Message,
	})

	switch {
	case state.addon.Status.LastAppliedChart == nil:
		apimeta.SetStatusCondition(&state.addon.Status.Conditions, metav1.Condition{
			Type:               ConditionTypeInstalled,
			Status:             metav1.ConditionFalse,
			Reason:             helmChartReadyCond.Reason,
			ObservedGeneration: state.addon.Generation,
			Message:            helmChartReadyCond.Message,
		})
	case !reflect.DeepEqual(state.addon.Status.LastAppliedValues, state.addon.Spec.Values):
		apimeta.SetStatusCondition(&state.addon.Status.Conditions, metav1.Condition{
			Type:               ConditionTypeConfigurationApplied,
			Status:             metav1.ConditionFalse,
			Reason:             helmChartReadyCond.Reason,
			ObservedGeneration: state.addon.Generation,
			Message:            helmChartReadyCond.Message,
		})
	default:
		apimeta.SetStatusCondition(&state.addon.Status.Conditions, metav1.Condition{
			Type:               ConditionTypeUpdateInstalled,
			Status:             metav1.ConditionFalse,
			Reason:             helmChartReadyCond.Reason,
			ObservedGeneration: state.addon.Generation,
			Message:            helmChartReadyCond.Message,
		})
	}

	state.addon.Status.ObservedGeneration = state.addon.Generation

	if err := r.client.Status().Patch(ctx, state.addon, client.MergeFrom(state.base)); err != nil {
		return reconcile.Result{}, fmt.Errorf("patching status on error: %w", err)
	}

	return reconcile.Result{RequeueAfter: ReconcileRetryInterval}, nil
}

func (r *reconciler) ensureResourceDeleted(ctx context.Context, name, namespace string, obj client.Object) error {
	if err := r.client.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, obj); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}

		return fmt.Errorf("checking existence of obsolete resource: %w", err)
	}

	if err := r.client.Delete(ctx, obj); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("deleting obsolete resource: %w", err)
	}

	return nil
}

func isLastAppliedChartUpdateRequired(addon *helmv1alpha1.HelmClusterAddon, internalHelmChart *sourcev1.HelmChart, internalHelmRelease *helmv2.HelmRelease) bool {
	if internalHelmRelease.Status.ObservedGeneration != internalHelmRelease.Generation {
		return false
	}

	if internalHelmChart != nil {
		if internalHelmChart.Status.ObservedGeneration != internalHelmChart.Generation {
			return false
		}

		if !apimeta.IsStatusConditionPresentAndEqual(internalHelmChart.Status.Conditions, ConditionTypeReady, metav1.ConditionTrue) {
			return false
		}
	}

	if apimeta.IsStatusConditionPresentAndEqual(internalHelmRelease.Status.Conditions, ConditionTypeReady, metav1.ConditionTrue) &&
		internalHelmRelease.Status.History.Len() > 1 {
		latest := internalHelmRelease.Status.History.Latest()
		previous := internalHelmRelease.Status.History.Previous(true)

		if previous != nil && previous.Status == "superseded" &&
			latest != nil &&
			(latest.VersionedChartName() != previous.VersionedChartName() ||
				addon.Spec.Chart.HelmClusterAddonRepository != addon.Status.LastAppliedChart.HelmClusterAddonRepository) {
			return true
		}
	}

	return false
}
