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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
	sourcev1 "github.com/werf/nelm-source-controller/api/v1"
)

// Reconciler reconciles HelmClusterRepository objects by mirroring them
// to namespaced HelmRepository resources in the target namespace.
type Reconciler struct {
	Client client.Client
}

// Reconcile implements reconcile.Reconciler.
func (r *Reconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	logger := log.FromContext(ctx).WithValues("helmclusterrepository", req.Name)

	var clusterRepo helmv1alpha1.HelmClusterAddonRepository
	if err := r.Client.Get(ctx, req.NamespacedName, &clusterRepo); err != nil {
		if apierrors.IsNotFound(err) {
			logger.Info("HelmClusterAddonRepository not found, skipping")
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, fmt.Errorf("getting HelmClusterAddonRepository: %w", err)
	}

	if !clusterRepo.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, &clusterRepo)
	}

	if !controllerutil.ContainsFinalizer(&clusterRepo, FinalizerName) {
		controllerutil.AddFinalizer(&clusterRepo, FinalizerName)
		if err := r.Client.Update(ctx, &clusterRepo); err != nil {
			return reconcile.Result{}, fmt.Errorf("adding finalizer: %w", err)
		}
	}

	desired := BuildDesiredHelmRepository(&clusterRepo)

	var existing sourcev1.HelmRepository
	err := r.Client.Get(ctx, types.NamespacedName{
		Name:      desired.Name,
		Namespace: desired.Namespace,
	}, &existing)

	if apierrors.IsNotFound(err) {
		logger.Info("Creating internal repository custom resource", "name", desired.Name, "namespace", desired.Namespace)
		if err := r.Client.Create(ctx, desired); err != nil {
			r.setCondition(&clusterRepo, metav1.ConditionFalse, ReasonMirrorFailed,
				fmt.Sprintf("Failed to create internal custom resource: %v", err))
			_ = r.Client.Status().Update(ctx, &clusterRepo)
			return reconcile.Result{}, fmt.Errorf("creating internal repository custom resource: %w", err)
		}
		// After creation, the internal resource has no status yet.
		r.setCondition(&clusterRepo, metav1.ConditionUnknown, ReasonMirrorSucceeded,
			"Internal repository custom resource created, waiting for status")
		if err := r.Client.Status().Update(ctx, &clusterRepo); err != nil {
			return reconcile.Result{}, fmt.Errorf("updating status after create: %w", err)
		}
		return reconcile.Result{}, nil
	}
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("getting internal repository custom resource: %w", err)
	}

	if ApplyDesiredSpec(&existing, desired) {
		logger.Info("Updating internal repository custom resource spec", "name", existing.Name)
		if err := r.Client.Update(ctx, &existing); err != nil {
			r.setCondition(&clusterRepo, metav1.ConditionFalse, ReasonMirrorFailed,
				fmt.Sprintf("Failed to update internal repository custom resource: %v", err))
			_ = r.Client.Status().Update(ctx, &clusterRepo)
			return reconcile.Result{}, fmt.Errorf("updating internal custom resource: %w", err)
		}
	}

	// 7. Propagate status from internal → cluster.
	conditions := MapInternalStatusToClusterConditions(&existing)
	clusterRepo.Status.Conditions = conditions
	clusterRepo.Status.ObservedGeneration = clusterRepo.Generation
	if err := r.Client.Status().Update(ctx, &clusterRepo); err != nil {
		return reconcile.Result{}, fmt.Errorf("updating internal custom resource status: %w", err)
	}

	return reconcile.Result{}, nil
}

// reconcileDelete handles cleanup when the HelmClusterRepository is being deleted.
func (r *Reconciler) reconcileDelete(ctx context.Context, clusterRepo *helmv1alpha1.HelmClusterAddonRepository) (reconcile.Result, error) {
	logger := log.FromContext(ctx).WithValues("helmclusteraddonrepository", clusterRepo.Name)

	if !controllerutil.ContainsFinalizer(clusterRepo, FinalizerName) {
		return reconcile.Result{}, nil
	}

	// Delete the internal repository resource.
	var internal sourcev1.HelmRepository
	err := r.Client.Get(ctx, types.NamespacedName{
		Name:      clusterRepo.Name,
		Namespace: TargetNamespace,
	}, &internal)

	if err == nil {
		logger.Info("Deleting internal repository resource", "name", internal.Name, "namespace", internal.Namespace)
		if err := r.Client.Delete(ctx, &internal); err != nil && !apierrors.IsNotFound(err) {
			r.setCondition(clusterRepo, metav1.ConditionFalse, ReasonCleanupFailed,
				fmt.Sprintf("Failed to delete internal repository resource: %v", err))
			_ = r.Client.Status().Update(ctx, clusterRepo)
			return reconcile.Result{}, fmt.Errorf("deleting internal repository resource: %w", err)
		}
	} else if !apierrors.IsNotFound(err) {
		return reconcile.Result{}, fmt.Errorf("getting internal repository resource for deletion: %w", err)
	}

	// Remove finalizer.
	controllerutil.RemoveFinalizer(clusterRepo, FinalizerName)
	if err := r.Client.Update(ctx, clusterRepo); err != nil {
		return reconcile.Result{}, fmt.Errorf("removing finalizer: %w", err)
	}

	logger.Info("Cleanup complete")
	return reconcile.Result{}, nil
}

// setCondition is a helper to set a single Ready condition on the cluster resource.
func (r *Reconciler) setCondition(repo *helmv1alpha1.HelmClusterAddonRepository, status metav1.ConditionStatus, reason, message string) {
	now := metav1.Now()
	newCond := metav1.Condition{
		Type:               ConditionTypeReady,
		Status:             status,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: now,
		ObservedGeneration: repo.Generation,
	}

	// Replace existing Ready condition or append.
	for i, c := range repo.Status.Conditions {
		if c.Type == ConditionTypeReady {
			// Only update LastTransitionTime if status actually changed.
			if c.Status == status {
				newCond.LastTransitionTime = c.LastTransitionTime
			}
			repo.Status.Conditions[i] = newCond
			return
		}
	}
	repo.Status.Conditions = append(repo.Status.Conditions, newCond)
}
