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
	"errors"
	"fmt"
	"net/url"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
	"github.com/werf/3p-fluxcd-pkg/apis/meta"
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

	var repo helmv1alpha1.HelmClusterAddonRepository

	if err := r.Client.Get(ctx, req.NamespacedName, &repo); err != nil {
		if apierrors.IsNotFound(err) {
			logger.Info("HelmClusterAddonRepository not found, skipping")

			return reconcile.Result{}, nil
		}

		return reconcile.Result{}, fmt.Errorf("getting HelmClusterAddonRepository: %w", err)
	}

	parsedURL, err := url.Parse(repo.Spec.URL)
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("cannot parse HelmClusterAddonRepository url: %w", err)
	}

	repoType, err := GetRepositoryType(parsedURL.Scheme)
	if err != nil {
		return reconcile.Result{}, err
	}

	if !repo.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, &repo, repoType)
	}

	if !controllerutil.ContainsFinalizer(&repo, FinalizerName) {
		controllerutil.AddFinalizer(&repo, FinalizerName)

		if err := r.Client.Update(ctx, &repo); err != nil {
			return reconcile.Result{}, fmt.Errorf("adding finalizer: %w", err)
		}

		return reconcile.Result{}, nil
	}

	switch repoType {
	case InternalHelmRepository:
		return r.reconcileInternalHelmRepository(ctx, &repo)
	case InternalOCIRepository:
		return r.reconcileInternalOCIRepository(ctx, &repo)
	default:
		return reconcile.Result{}, nil
	}
}

func (r *Reconciler) reconcileInternalHelmRepository(ctx context.Context, repo *helmv1alpha1.HelmClusterAddonRepository) (reconcile.Result, error) {
	logger := log.FromContext(ctx)

	if err := r.reconcileInternalRepositoryAuthSecret(ctx, repo, InternalHelmRepository); err != nil {
		return reconcile.Result{}, err
	}

	if err := r.reconcileInternalRepositoryTLSSecret(ctx, repo, InternalHelmRepository); err != nil {
		return reconcile.Result{}, err
	}

	existing := &sourcev1.HelmRepository{
		ObjectMeta: metav1.ObjectMeta{
			Name:      repo.Name,
			Namespace: TargetNamespace,
		},
	}

	op, err := controllerutil.CreateOrPatch(ctx, r.Client, existing, func() error {
		existing.Spec.URL = repo.Spec.URL
		existing.Spec.Interval = metav1.Duration{Duration: DefaultInterval}
		existing.Spec.Insecure = !repo.Spec.TLSVerify
		existing.Spec.CertSecretRef = nil
		existing.Spec.SecretRef = nil

		if repo.Spec.Auth != nil {
			existing.Spec.SecretRef = &meta.LocalObjectReference{
				Name: GetRepositoryAuthSecretName(InternalHelmRepository, repo.Name),
			}
			existing.Spec.PassCredentials = true
		}

		if repo.Spec.CACertificate != "" {
			existing.Spec.CertSecretRef = &meta.LocalObjectReference{
				Name: GetRepositoryTLSSecretName(InternalHelmRepository, repo.Name),
			}
		}

		existing.Labels = map[string]string{
			LabelManagedBy:  LabelManagedByValue,
			LabelSourceName: repo.Name,
		}

		return nil
	})
	if err != nil {
		return reconcile.Result{}, r.patchStatusError(ctx, repo, fmt.Errorf("reconciling helm repository: %w", err), ReasonMirrorFailed)
	}

	if op != controllerutil.OperationResultNone {
		logger.Info("Successfully reconciled helm repository", "operation", op)
	}

	return r.updateSuccessStatus(ctx, repo, existing.Status.Conditions)
}

func (r *Reconciler) reconcileInternalOCIRepository(ctx context.Context, repo *helmv1alpha1.HelmClusterAddonRepository) (reconcile.Result, error) {
	logger := log.FromContext(ctx)

	if err := r.reconcileInternalRepositoryAuthSecret(ctx, repo, InternalOCIRepository); err != nil {
		return reconcile.Result{}, err
	}

	if err := r.reconcileInternalRepositoryTLSSecret(ctx, repo, InternalOCIRepository); err != nil {
		return reconcile.Result{}, err
	}

	existing := &sourcev1.OCIRepository{
		ObjectMeta: metav1.ObjectMeta{
			Name:      repo.Name,
			Namespace: TargetNamespace,
		},
	}

	op, err := controllerutil.CreateOrPatch(ctx, r.Client, existing, func() error {
		existing.Spec.URL = repo.Spec.URL
		existing.Spec.Interval = metav1.Duration{Duration: DefaultInterval}
		existing.Spec.Insecure = !repo.Spec.TLSVerify
		existing.Spec.CertSecretRef = nil
		existing.Spec.SecretRef = nil

		if repo.Spec.Auth != nil {
			existing.Spec.SecretRef = &meta.LocalObjectReference{
				Name: GetRepositoryAuthSecretName(InternalOCIRepository, repo.Name),
			}
		}

		if repo.Spec.CACertificate != "" {
			existing.Spec.CertSecretRef = &meta.LocalObjectReference{
				Name: GetRepositoryTLSSecretName(InternalOCIRepository, repo.Name),
			}
		}

		existing.Labels = map[string]string{
			LabelManagedBy:  LabelManagedByValue,
			LabelSourceName: repo.Name,
		}

		return nil
	})
	if err != nil {
		return reconcile.Result{}, r.patchStatusError(ctx, repo, fmt.Errorf("reconciling oci repository: %w", err), ReasonMirrorFailed)
	}

	if op != controllerutil.OperationResultNone {
		logger.Info("Successfully reconciled oci repository", "operation", op)
	}

	return r.updateSuccessStatus(ctx, repo, existing.Status.Conditions)
}

func (r *Reconciler) reconcileInternalRepositoryAuthSecret(ctx context.Context, repo *helmv1alpha1.HelmClusterAddonRepository, repoType InternalRepositoryType) error {
	secretName := GetRepositoryAuthSecretName(repoType, repo.Name)

	if repo.Spec.Auth == nil {
		if err := r.ensureResourceDeleted(ctx, secretName, TargetNamespace, &corev1.Secret{}); err != nil {
			return fmt.Errorf("cannot delete obsolete auth secret: %w", err)
		}

		return nil
	}

	authSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: TargetNamespace,
		},
	}

	if _, err := controllerutil.CreateOrPatch(ctx, r.Client, authSecret, func() error {
		authSecret.Labels = map[string]string{
			LabelManagedBy:  LabelManagedByValue,
			LabelSourceName: repo.Name,
		}

		authSecret.StringData = map[string]string{
			"username": repo.Spec.Auth.Username,
			"password": repo.Spec.Auth.Password,
		}

		return nil
	}); err != nil {
		return fmt.Errorf("cannot reconcile auth secret: %w", err)
	}

	return nil
}

func (r *Reconciler) reconcileInternalRepositoryTLSSecret(ctx context.Context, repo *helmv1alpha1.HelmClusterAddonRepository, repoType InternalRepositoryType) error {
	secretName := GetRepositoryTLSSecretName(repoType, repo.Name)

	if repo.Spec.CACertificate == "" {
		if err := r.ensureResourceDeleted(ctx, secretName, TargetNamespace, &corev1.Secret{}); err != nil {
			return fmt.Errorf("cannot delete obsolete tls secret: %w", err)
		}

		return nil
	}

	// TODO: consider adding CA certificate format validation

	tlsSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: TargetNamespace,
		},
	}

	if _, err := controllerutil.CreateOrPatch(ctx, r.Client, tlsSecret, func() error {
		tlsSecret.Labels = map[string]string{
			LabelManagedBy:  LabelManagedByValue,
			LabelSourceName: repo.Name,
		}

		tlsSecret.StringData = map[string]string{
			"ca.crt": repo.Spec.CACertificate,
		}

		return nil
	}); err != nil {
		return fmt.Errorf("cannot reconcile tls secret: %w", err)
	}

	return nil
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
func (r *Reconciler) reconcileDelete(ctx context.Context, repo *helmv1alpha1.HelmClusterAddonRepository, repoType InternalRepositoryType) (reconcile.Result, error) {
	logger := log.FromContext(ctx).WithValues("helmclusteraddonrepository", repo.Name)

	if !controllerutil.ContainsFinalizer(repo, FinalizerName) {
		return reconcile.Result{}, nil
	}

	if err := r.ensureResourceDeleted(
		ctx,
		GetRepositoryAuthSecretName(repoType, repo.Name),
		TargetNamespace,
		&corev1.Secret{},
	); err != nil {
		return reconcile.Result{}, r.patchStatusError(ctx, repo, fmt.Errorf("deleting internal auth secret: %w", err), ReasonCleanupFailed)
	}

	if err := r.ensureResourceDeleted(
		ctx,
		GetRepositoryTLSSecretName(repoType, repo.Name),
		TargetNamespace,
		&corev1.Secret{},
	); err != nil {
		return reconcile.Result{}, r.patchStatusError(ctx, repo, fmt.Errorf("deleting internal tls secret: %w", err), ReasonCleanupFailed)
	}

	var internalRepository client.Object

	switch repoType {
	case InternalHelmRepository:
		internalRepository = &sourcev1.HelmRepository{}
	case InternalOCIRepository:
		internalRepository = &sourcev1.OCIRepository{}
	default:
		return reconcile.Result{}, r.patchStatusError(ctx, repo, fmt.Errorf("cannot remove unsupported repisotory type: %s", repoType), ReasonCleanupFailed)
	}

	if err := r.ensureResourceDeleted(ctx, repo.Name, TargetNamespace, internalRepository); err != nil {
		return reconcile.Result{}, r.patchStatusError(ctx, repo, fmt.Errorf("deleting internal repository: %w", err), ReasonCleanupFailed)
	}

	controllerutil.RemoveFinalizer(repo, FinalizerName)

	if err := r.Client.Update(ctx, repo); err != nil {
		return reconcile.Result{}, fmt.Errorf("removing finalizer: %w", err)
	}

	logger.Info("Cleanup complete")

	return reconcile.Result{}, nil
}

// patchStatusError is a helper to safely patch a failure condition onto the cluster resource.
func (r *Reconciler) patchStatusError(ctx context.Context, repo *helmv1alpha1.HelmClusterAddonRepository, reconcileErr error, reason string) error {
	base := repo.DeepCopy()

	r.setCondition(repo, metav1.ConditionFalse, reason, reconcileErr.Error())

	if patchErr := r.Client.Status().Patch(ctx, repo, client.MergeFrom(base)); patchErr != nil {
		return errors.Join(reconcileErr, fmt.Errorf("failed to patch status: %w", patchErr))
	}

	return reconcileErr
}

// updateSuccessStatus patches the status of the cluster resource after a successful reconciliation.
func (r *Reconciler) updateSuccessStatus(ctx context.Context, repo *helmv1alpha1.HelmClusterAddonRepository, internalConditions []metav1.Condition) (reconcile.Result, error) {
	base := repo.DeepCopy()

	repo.Status.Conditions = MapInternalStatusToClusterConditions(internalConditions)
	repo.Status.ObservedGeneration = repo.Generation

	if err := r.Client.Status().Patch(ctx, repo, client.MergeFrom(base)); err != nil {
		return reconcile.Result{}, fmt.Errorf("patching internal custom resource status: %w", err)
	}

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
