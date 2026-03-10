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
	"github.com/deckhouse/operator-helm/pkg/controller/helmclusteraddonchart"
	"github.com/deckhouse/operator-helm/pkg/utils"
)

type Reconciler struct {
	Client client.Client
}

type ReconcileContext struct {
	addon                  *helmv1alpha1.HelmClusterAddon
	addonBase              *helmv1alpha1.HelmClusterAddon
	addonChart             *helmv1alpha1.HelmClusterAddonChart
	addonRepository        *helmv1alpha1.HelmClusterAddonRepository
	internalHelmRelease    *helmv2.HelmRelease
	internalHelmChart      *sourcev1.HelmChart
	maintenanceModeEnabled bool
	err                    []error
}

func (r *ReconcileContext) AddonDeepCopy() *helmv1alpha1.HelmClusterAddon {
	if r.addonBase == nil {
		r.addonBase = r.addon.DeepCopy()
	}

	return r.addonBase
}

func (r *ReconcileContext) GetRepositoryType() (utils.InternalRepositoryType, error) {
	return utils.GetRepositoryType(r.addonRepository.Spec.URL)
}

type pipelineStep struct {
	Name          string
	RunIf         func(rctx *ReconcileContext) bool
	Action        func(ctx context.Context, rctx *ReconcileContext) (pipelineStepResult, error)
	StopOnFailure bool
	SkipOnError   bool
}

type pipelineStepResult struct {
	Requeue bool
}

func (s *pipelineStepResult) Reconcile() reconcile.Result {
	if s.Requeue {
		return reconcile.Result{RequeueAfter: ReconcileRetryInterval}
	}

	return reconcile.Result{}
}

func (r *Reconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	logger := log.FromContext(ctx).WithValues("helmclusteraddon", req.Name)

	rctx := &ReconcileContext{addon: &helmv1alpha1.HelmClusterAddon{}}

	if err := r.Client.Get(ctx, req.NamespacedName, rctx.addon); err != nil {
		if apierrors.IsNotFound(err) {
			logger.Info("HelmClusterAddon not found, skipping")

			return reconcile.Result{}, nil
		}

		return reconcile.Result{}, fmt.Errorf("getting HelmClusterAddon: %w", err)
	}

	rctx.AddonDeepCopy()

	pipeline := []pipelineStep{
		{
			Name: "Ensure that conditions initialized",
			RunIf: func(rctx *ReconcileContext) bool {
				return apimeta.FindStatusCondition(rctx.addon.Status.Conditions, ConditionTypeReady) == nil
			},
			Action:        r.initializeConditions,
			StopOnFailure: true,
		},
		{
			Name: "Delete resources if deletion timestamp present",
			RunIf: func(rctx *ReconcileContext) bool {
				return !rctx.addon.DeletionTimestamp.IsZero()
			},
			Action:        r.reconcileDelete,
			StopOnFailure: true,
		},
		{
			Name: "Add finalizer if it is absent",
			RunIf: func(rctx *ReconcileContext) bool {
				return !controllerutil.ContainsFinalizer(rctx.addon, FinalizerName)
			},
			Action:        r.addFinalizer,
			StopOnFailure: true,
		},
		{
			Name:          "Stop if maintenance mode is enabled",
			RunIf:         func(rctx *ReconcileContext) bool { return true },
			Action:        r.checkIfMaintenanceModeEnabled,
			StopOnFailure: true,
		},
		{
			Name:          "Set status to Processing if needed",
			RunIf:         func(rctx *ReconcileContext) bool { return !rctx.maintenanceModeEnabled },
			Action:        r.setStatusToProcessing,
			StopOnFailure: true,
		},
		{
			Name:          "Get HelmClusterAddonRepository",
			RunIf:         func(rctx *ReconcileContext) bool { return !rctx.maintenanceModeEnabled },
			Action:        r.getHelmClusterAddonRepository,
			StopOnFailure: true,
		},
		{
			Name:        "Reconcile InternalNelmOperatorHelmChart",
			RunIf:       func(rctx *ReconcileContext) bool { return !rctx.maintenanceModeEnabled },
			Action:      r.reconcileInternalHelmChart,
			SkipOnError: true,
		},
		{
			Name: "Reconcile InternalNelmOperatorHelmRelease",
			RunIf: func(rctx *ReconcileContext) bool {
				return !rctx.maintenanceModeEnabled && rctx.internalHelmChart != nil && rctx.internalHelmChart.Status.Artifact != nil
			},
			Action:      r.reconcileInternalHelmRelease,
			SkipOnError: true,
		},
		{
			Name:   "Update HelmClusterAddon status",
			RunIf:  func(rctx *ReconcileContext) bool { return true },
			Action: r.updateStatus,
		},
	}

	for _, step := range pipeline {
		if step.RunIf(rctx) {
			logger.Info("Running step", "step", step.Name)

			if len(rctx.err) > 0 && step.SkipOnError {
				logger.Info("Step skipped due to error on the previous step", "step", step.Name)

				continue
			}

			if result, err := step.Action(ctx, rctx); err != nil {
				if step.StopOnFailure {
					return reconcile.Result{}, err
				}

				rctx.err = append(rctx.err, err)
			} else if result.Requeue {
				return result.Reconcile(), nil
			}

			logger.Info("Step completed", "step", step.Name)

			continue
		}

		logger.Info("Skipping optional step", "step", step.Name)
	}

	return reconcile.Result{}, nil
}

func (r *Reconciler) initializeConditions(ctx context.Context, rctx *ReconcileContext) (pipelineStepResult, error) {

	conditionTypes := []string{
		ConditionTypeReady,
		ConditionTypeManaged,
	}

	for _, t := range conditionTypes {
		if apimeta.FindStatusCondition(rctx.addon.Status.Conditions, t) == nil {
			apimeta.SetStatusCondition(&rctx.addon.Status.Conditions, metav1.Condition{
				Type:   t,
				Status: metav1.ConditionUnknown,
				Reason: ReasonInitializing,
			})
		}
	}

	if err := r.Client.Status().Update(ctx, rctx.addon); err != nil {
		return pipelineStepResult{}, fmt.Errorf("failed to update helmclusteraddon status conditions: %w", err)
	}

	return pipelineStepResult{Requeue: true}, nil
}

func (r *Reconciler) reconcileDelete(ctx context.Context, rctx *ReconcileContext) (pipelineStepResult, error) {
	logger := log.FromContext(ctx).WithValues("helmclusteraddon", rctx.addon.Name)

	if !controllerutil.ContainsFinalizer(rctx.addon, FinalizerName) {
		return pipelineStepResult{}, nil
	}

	if err := r.ensureResourceDeleted(ctx, utils.GetInternalHelmReleaseName(rctx.addon.Name), TargetNamespace, &helmv2.HelmRelease{}); err != nil {
		return pipelineStepResult{}, fmt.Errorf("deleting internal helm release: %w", err)
	}

	if err := r.ensureResourceDeleted(ctx, utils.GetInternalHelmChartName(rctx.addon.Name), TargetNamespace, &sourcev1.HelmChart{}); err != nil {
		return pipelineStepResult{}, fmt.Errorf("deleting internal helm chart: %w", err)
	}

	controllerutil.RemoveFinalizer(rctx.addon, FinalizerName)

	if err := r.Client.Update(ctx, rctx.addon); err != nil {
		return pipelineStepResult{}, fmt.Errorf("removing finalizer: %w", err)
	}

	logger.Info("Cleanup complete")

	return pipelineStepResult{}, nil
}

func (r *Reconciler) addFinalizer(ctx context.Context, rctx *ReconcileContext) (pipelineStepResult, error) {
	controllerutil.AddFinalizer(rctx.addon, FinalizerName)

	if err := r.Client.Update(ctx, rctx.addon); err != nil {
		return pipelineStepResult{}, fmt.Errorf("failed to add finalizer to helm cluster addon: %w", err)
	}

	return pipelineStepResult{Requeue: true}, nil
}

func (r *Reconciler) checkIfMaintenanceModeEnabled(_ context.Context, rctx *ReconcileContext) (pipelineStepResult, error) {
	managedCond := apimeta.FindStatusCondition(rctx.addon.Status.Conditions, ConditionTypeManaged)

	if managedCond == nil {
		return pipelineStepResult{}, fmt.Errorf("managed condition is not initialized")
	} else if managedCond.Status == metav1.ConditionFalse && rctx.addon.Spec.Maintenance == string(helmv1alpha1.NoResourceReconciliation) {
		rctx.maintenanceModeEnabled = true
	}

	return pipelineStepResult{}, nil
}

func (r *Reconciler) setStatusToProcessing(ctx context.Context, rctx *ReconcileContext) (pipelineStepResult, error) {
	readyCond := apimeta.FindStatusCondition(rctx.addon.Status.Conditions, ConditionTypeReady)

	if rctx.addon.Generation == rctx.addon.Status.ObservedGeneration ||
		(readyCond != nil && readyCond.Status == metav1.ConditionFalse && readyCond.Reason == ReasonProcessing) {
		return pipelineStepResult{}, nil
	}

	chartChanged := isChartSpecChanged(rctx.addon)

	valuesChanged := false
	specRaw := ""
	if rctx.addon.Spec.Values != nil {
		specRaw = string(rctx.addon.Spec.Values.Raw)
	}
	lastRaw := ""
	if rctx.addon.Status.LastAppliedValues != nil {
		lastRaw = string(rctx.addon.Status.LastAppliedValues.Raw)
	}
	if specRaw != lastRaw {
		valuesChanged = true
	}

	if rctx.addon.Status.LastAppliedChart == nil {
		apimeta.SetStatusCondition(&rctx.addon.Status.Conditions, metav1.Condition{
			Type:               ConditionTypeInstalled,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: rctx.addon.Generation,
			Reason:             ReasonInstallationInProgress,
			Message:            "",
		})
	} else {
		if chartChanged {
			apimeta.SetStatusCondition(&rctx.addon.Status.Conditions, metav1.Condition{
				Type:               ConditionTypeUpdateInstalled,
				Status:             metav1.ConditionFalse,
				ObservedGeneration: rctx.addon.Generation,
				Reason:             ReasonUpdateInProgress,
				Message:            "",
			})
		}
		if valuesChanged {
			apimeta.SetStatusCondition(&rctx.addon.Status.Conditions, metav1.Condition{
				Type:               ConditionTypeConfigurationApplied,
				Status:             metav1.ConditionFalse,
				ObservedGeneration: rctx.addon.Generation,
				Reason:             ReasonUpdateInProgress,
				Message:            "",
			})
		}

		// Neither chart nor values changed (e.g. annotation bump) — treat as values-only change.
		if !chartChanged && !valuesChanged {
			apimeta.SetStatusCondition(&rctx.addon.Status.Conditions, metav1.Condition{
				Type:               ConditionTypeConfigurationApplied,
				Status:             metav1.ConditionFalse,
				ObservedGeneration: rctx.addon.Generation,
				Reason:             ReasonUpdateInProgress,
				Message:            "",
			})
		}
	}

	apimeta.SetStatusCondition(&rctx.addon.Status.Conditions, metav1.Condition{
		Type:               ConditionTypeReady,
		Status:             metav1.ConditionFalse,
		ObservedGeneration: rctx.addon.Generation,
		Reason:             ReasonProcessing,
		Message:            "",
	})

	if err := r.Client.Status().Patch(ctx, rctx.addon, client.MergeFrom(rctx.AddonDeepCopy())); err != nil {
		return pipelineStepResult{}, fmt.Errorf("updating helm cluster addon status: %w", err)
	}

	return pipelineStepResult{Requeue: true}, nil
}

func (r *Reconciler) getHelmClusterAddonRepository(ctx context.Context, rctx *ReconcileContext) (pipelineStepResult, error) {
	rctx.addonRepository = &helmv1alpha1.HelmClusterAddonRepository{}

	if err := r.Client.Get(ctx, types.NamespacedName{Name: rctx.addon.Spec.Chart.HelmClusterAddonRepository}, rctx.addonRepository); err != nil {
		if apierrors.IsNotFound(err) {
			return pipelineStepResult{}, fmt.Errorf("helm cluster addon repository not found: %w", err)
		}

		return pipelineStepResult{}, fmt.Errorf("getting helm cluster addon repository: %w", err)
	}

	return pipelineStepResult{}, nil
}

func (r *Reconciler) reconcileInternalHelmChart(ctx context.Context, rctx *ReconcileContext) (pipelineStepResult, error) {
	logger := log.FromContext(ctx)

	repoType, err := rctx.GetRepositoryType()
	if err != nil {
		return pipelineStepResult{}, fmt.Errorf("getting repository type: %w", err)
	}

	rctx.internalHelmChart = &sourcev1.HelmChart{
		ObjectMeta: metav1.ObjectMeta{
			Name:      utils.GetInternalHelmChartName(rctx.addon.Name),
			Namespace: TargetNamespace,
		},
	}

	op, err := controllerutil.CreateOrPatch(ctx, r.Client, rctx.internalHelmChart, func() error {
		if rctx.internalHelmChart.Labels == nil {
			rctx.internalHelmChart.Labels = map[string]string{}
		}

		rctx.internalHelmChart.Labels[LabelManagedBy] = LabelManagedByValue
		rctx.internalHelmChart.Labels[LabelSourceName] = rctx.addon.Name
		rctx.internalHelmChart.Labels[helmclusteraddonchart.LabelSourceName] = utils.GetHelmClusterAddonChartName(
			rctx.addon.Spec.Chart.HelmClusterAddonRepository, rctx.addon.Spec.Chart.HelmClusterAddonChartName)

		rctx.internalHelmChart.Spec.Chart = rctx.addon.Spec.Chart.HelmClusterAddonChartName
		rctx.internalHelmChart.Spec.Version = rctx.addon.Spec.Chart.Version

		switch repoType {
		case utils.InternalHelmRepository:
			rctx.internalHelmChart.Spec.SourceRef = sourcev1.LocalHelmChartSourceReference{
				Kind: sourcev1.HelmRepositoryKind,
				Name: rctx.addon.Spec.Chart.HelmClusterAddonRepository,
			}
		case utils.InternalOCIRepository:
			rctx.internalHelmChart.Spec.SourceRef = sourcev1.LocalHelmChartSourceReference{
				Kind: sourcev1.OCIRepositoryKind,
				Name: rctx.addon.Spec.Chart.HelmClusterAddonRepository,
			}
		default:
			return fmt.Errorf("invalid repository type: %s", repoType)
		}

		return nil
	})
	if err != nil {
		return pipelineStepResult{}, fmt.Errorf("cannot create or update internal nelm operator helm chart: %w", err)
	}

	if rctx.addon.Spec.Maintenance == string(helmv1alpha1.NoResourceReconciliation) {
		apimeta.SetStatusCondition(&rctx.addon.Status.Conditions, metav1.Condition{
			Type:    ConditionTypeManaged,
			Status:  metav1.ConditionFalse,
			Reason:  ReasonUnmanagedModeActivated,
			Message: "",
		})
	} else {
		apimeta.SetStatusCondition(&rctx.addon.Status.Conditions, metav1.Condition{
			Type:    ConditionTypeManaged,
			Status:  metav1.ConditionTrue,
			Reason:  ReasonManagedModeActivated,
			Message: "",
		})
	}

	if op != controllerutil.OperationResultNone {
		logger.Info("Successfully reconciled internal nelm operator helm chart", "operation", op, "repository", rctx.addon.Spec.Chart.HelmClusterAddonRepository, "chart", rctx.addon.Spec.Chart.HelmClusterAddonChartName)
	}

	return pipelineStepResult{}, nil
}

func (r *Reconciler) reconcileInternalHelmRelease(ctx context.Context, rctx *ReconcileContext) (pipelineStepResult, error) {
	logger := log.FromContext(ctx)

	rctx.internalHelmRelease = &helmv2.HelmRelease{
		ObjectMeta: metav1.ObjectMeta{
			Name:      utils.GetInternalHelmReleaseName(rctx.addon.Name),
			Namespace: TargetNamespace,
		},
	}

	op, err := controllerutil.CreateOrPatch(ctx, r.Client, rctx.internalHelmRelease, func() error {
		if rctx.internalHelmRelease.Labels == nil {
			rctx.internalHelmRelease.Labels = map[string]string{}
		}

		rctx.internalHelmRelease.Labels[LabelManagedBy] = LabelManagedByValue
		rctx.internalHelmRelease.Labels[LabelSourceName] = rctx.addon.Name

		rctx.internalHelmRelease.Spec.ReleaseName = rctx.addon.Name
		rctx.internalHelmRelease.Spec.TargetNamespace = rctx.addon.Spec.Namespace
		rctx.internalHelmRelease.Spec.Values = rctx.addon.Spec.Values

		rctx.internalHelmRelease.Spec.Suspend = false

		if rctx.addon.Spec.Maintenance == string(helmv1alpha1.NoResourceReconciliation) {
			rctx.internalHelmRelease.Spec.Suspend = true
		}

		rctx.internalHelmRelease.Spec.ChartRef = &helmv2.CrossNamespaceSourceReference{
			Kind:      sourcev1.HelmChartKind,
			Name:      utils.GetInternalHelmChartName(rctx.addon.Name),
			Namespace: TargetNamespace,
		}

		return nil
	})
	if err != nil {
		return pipelineStepResult{}, fmt.Errorf("reconcile internal nelm operator helm release: %w", err)
	}

	if op != controllerutil.OperationResultNone {
		logger.Info("Successfully reconciled internal nelm operator helm release", "operation", op, "chart", rctx.addon.Spec.Chart.HelmClusterAddonChartName)
	}

	return pipelineStepResult{}, nil
}

func (r *Reconciler) updateStatus(ctx context.Context, rctx *ReconcileContext) (pipelineStepResult, error) {
	logger := log.FromContext(ctx)

	if rctx.maintenanceModeEnabled {
		return pipelineStepResult{}, nil
	}

	addonReadyCond := apimeta.FindStatusCondition(rctx.addon.Status.Conditions, ConditionTypeReady)
	if addonReadyCond == nil {
		return pipelineStepResult{Requeue: true}, nil
	}

	joinedErr := errors.Join(rctx.err...)

	if joinedErr != nil {
		apimeta.SetStatusCondition(&rctx.addon.Status.Conditions, metav1.Condition{
			Type:    ConditionTypeReady,
			Status:  metav1.ConditionFalse,
			Reason:  ReasonReconcileFailed,
			Message: joinedErr.Error(),
		})
		rctx.addon.Status.ObservedGeneration = rctx.addon.Generation

		if err := r.Client.Status().Patch(ctx, rctx.addon, client.MergeFrom(rctx.AddonDeepCopy())); err != nil {
			return pipelineStepResult{}, fmt.Errorf("updating HelmClusterAddon status on error: %w", err)
		}

		return pipelineStepResult{}, nil
	}

	if rctx.internalHelmRelease == nil {
		return pipelineStepResult{Requeue: true}, nil
	}

	if rctx.internalHelmRelease.Status.ObservedGeneration != rctx.internalHelmRelease.Generation {
		return pipelineStepResult{Requeue: true}, nil
	}

	helmReleaseReadyCond := apimeta.FindStatusCondition(rctx.internalHelmRelease.Status.Conditions, ConditionTypeReady)
	if helmReleaseReadyCond == nil {
		return pipelineStepResult{Requeue: true}, nil
	}

	apimeta.SetStatusCondition(&rctx.addon.Status.Conditions, metav1.Condition{
		Type:    ConditionTypeReady,
		Status:  helmReleaseReadyCond.Status,
		Reason:  helmReleaseReadyCond.Reason,
		Message: helmReleaseReadyCond.Message,
	})

	terminal := false

	helmChartReadyCond := apimeta.FindStatusCondition(rctx.internalHelmChart.Status.Conditions, ConditionTypeReady)
	if helmChartReadyCond != nil {
		if helmChartReadyCond.Status == metav1.ConditionTrue {
			apimeta.SetStatusCondition(&rctx.addon.Status.Conditions, metav1.Condition{
				Type:    ConditionTypePartiallyDegraded,
				Status:  metav1.ConditionFalse,
				Reason:  helmReleaseReadyCond.Reason,
				Message: "",
			})
		} else {
			apimeta.SetStatusCondition(&rctx.addon.Status.Conditions, metav1.Condition{
				Type:    ConditionTypePartiallyDegraded,
				Status:  metav1.ConditionTrue,
				Reason:  helmChartReadyCond.Reason,
				Message: "",
			})
		}
	}

	switch helmReleaseReadyCond.Reason {
	case helmv2.InstallSucceededReason:
		terminal = true
		apimeta.SetStatusCondition(&rctx.addon.Status.Conditions, metav1.Condition{
			Type:    ConditionTypeInstalled,
			Status:  metav1.ConditionTrue,
			Reason:  ReasonInstallSucceeded,
			Message: "",
		})

		// Remove UpdateInstalled if present from a prior chart-change cycle.
		apimeta.RemoveStatusCondition(&rctx.addon.Status.Conditions, ConditionTypeUpdateInstalled)

		if apimeta.IsStatusConditionPresentAndEqual(rctx.internalHelmChart.Status.Conditions, ConditionTypeReady, metav1.ConditionTrue) &&
			rctx.internalHelmChart.Generation == rctx.internalHelmChart.Generation {
			rctx.addon.Status.LastAppliedChart = &helmv1alpha1.HelmClusterAddonLastAppliedChartRef{
				HelmClusterAddonChartName:  rctx.addon.Spec.Chart.HelmClusterAddonChartName,
				HelmClusterAddonRepository: rctx.addon.Spec.Chart.HelmClusterAddonRepository,
				Version:                    rctx.addon.Spec.Chart.Version,
			}
		}

		rctx.addon.Status.LastAppliedValues = rctx.addon.Spec.Values
	case helmv2.UpgradeSucceededReason:
		terminal = true
		lastAppliedChartUpdateRequired := isLastAppliedChartUpdateRequired(rctx.addon, rctx.internalHelmChart, rctx.internalHelmRelease)

		if lastAppliedChartUpdateRequired {
			apimeta.SetStatusCondition(&rctx.addon.Status.Conditions, metav1.Condition{
				Type:    ConditionTypeUpdateInstalled,
				Status:  metav1.ConditionTrue,
				Reason:  ReasonUpdateSucceeded,
				Message: "",
			})
			rctx.addon.Status.LastAppliedChart = &helmv1alpha1.HelmClusterAddonLastAppliedChartRef{
				HelmClusterAddonChartName:  rctx.addon.Spec.Chart.HelmClusterAddonChartName,
				HelmClusterAddonRepository: rctx.addon.Spec.Chart.HelmClusterAddonRepository,
				Version:                    rctx.addon.Spec.Chart.Version,
			}
		}

		if rctx.addon.Spec.Values != nil {
			if addonValues, err := helmchartutil.ReadValues(rctx.addon.Spec.Values.Raw); err != nil {
				logger.Error(err, "failed to decode helm cluster addon values; marking ConfigurationApplied without digest verification")
				apimeta.SetStatusCondition(&rctx.addon.Status.Conditions, metav1.Condition{
					Type:    ConditionTypeConfigurationApplied,
					Status:  metav1.ConditionTrue,
					Reason:  ReasonUpdateSucceeded,
					Message: "",
				})
				rctx.addon.Status.LastAppliedValues = rctx.addon.Spec.Values
			} else {
				latestRelease := rctx.internalHelmRelease.Status.History.Latest()
				if latestRelease != nil && latestRelease.Status == InternalHelmReleaseDeployed &&
					latestRelease.ConfigDigest == chartutil.DigestValues(digest.Canonical, addonValues).String() {
					apimeta.SetStatusCondition(&rctx.addon.Status.Conditions, metav1.Condition{
						Type:    ConditionTypeConfigurationApplied,
						Status:  metav1.ConditionTrue,
						Reason:  ReasonUpdateSucceeded,
						Message: "Applied configuration with values digest " + latestRelease.ConfigDigest,
					})
					rctx.addon.Status.LastAppliedValues = rctx.addon.Spec.Values
				}
			}
		} else {
			apimeta.SetStatusCondition(&rctx.addon.Status.Conditions, metav1.Condition{
				Type:    ConditionTypeConfigurationApplied,
				Status:  metav1.ConditionTrue,
				Reason:  ReasonUpdateSucceeded,
				Message: "",
			})
			rctx.addon.Status.LastAppliedValues = nil
		}

	case helmv2.InstallFailedReason:
		terminal = true
		apimeta.SetStatusCondition(&rctx.addon.Status.Conditions, metav1.Condition{
			Type:    ConditionTypeInstalled,
			Status:  metav1.ConditionFalse,
			Reason:  ReasonInstallFailed,
			Message: helmReleaseReadyCond.Message,
		})

	case helmv2.UpgradeFailedReason:
		terminal = true
		if isChartSpecChanged(rctx.addon) {
			apimeta.SetStatusCondition(&rctx.addon.Status.Conditions, metav1.Condition{
				Type:    ConditionTypeUpdateInstalled,
				Status:  metav1.ConditionFalse,
				Reason:  ReasonUpdateFailed,
				Message: helmReleaseReadyCond.Message,
			})
		} else {
			apimeta.SetStatusCondition(&rctx.addon.Status.Conditions, metav1.Condition{
				Type:    ConditionTypeConfigurationApplied,
				Status:  metav1.ConditionFalse,
				Reason:  ReasonUpdateFailed,
				Message: helmReleaseReadyCond.Message,
			})
		}

	case helmv2.ArtifactFailedReason, helmv2.RollbackFailedReason, helmv2.UninstallFailedReason:
		terminal = true
	}

	if terminal {
		rctx.addon.Status.ObservedGeneration = rctx.addon.Generation
	}

	if err := r.Client.Status().Patch(ctx, rctx.addon, client.MergeFrom(rctx.AddonDeepCopy())); err != nil {
		return pipelineStepResult{}, fmt.Errorf("updating helm cluster addon status: %w", err)
	}

	if !terminal {
		return pipelineStepResult{Requeue: true}, nil
	}

	return pipelineStepResult{}, nil
}

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

func isLastAppliedChartUpdateRequired(addon *helmv1alpha1.HelmClusterAddon, internalHelmChart *sourcev1.HelmChart, internalHelmRelease *helmv2.HelmRelease) bool {
	if internalHelmChart.Status.ObservedGeneration != internalHelmChart.Generation {
		return false
	}

	if internalHelmRelease.Status.ObservedGeneration != internalHelmRelease.Generation {
		return false
	}

	if apimeta.IsStatusConditionPresentAndEqual(internalHelmChart.Status.Conditions, ConditionTypeReady, metav1.ConditionTrue) &&
		apimeta.IsStatusConditionPresentAndEqual(internalHelmRelease.Status.Conditions, ConditionTypeReady, metav1.ConditionTrue) &&
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

func isChartSpecChanged(addon *helmv1alpha1.HelmClusterAddon) bool {
	if addon.Status.LastAppliedChart == nil {
		return true
	}

	return addon.Spec.Chart.HelmClusterAddonChartName != addon.Status.LastAppliedChart.HelmClusterAddonChartName ||
		addon.Spec.Chart.HelmClusterAddonRepository != addon.Status.LastAppliedChart.HelmClusterAddonRepository ||
		addon.Spec.Chart.Version != addon.Status.LastAppliedChart.Version
}
