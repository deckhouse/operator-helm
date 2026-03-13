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

package services

import (
	"context"
	"fmt"
	"time"

	"github.com/werf/3p-fluxcd-pkg/apis/meta"
	sourcev1 "github.com/werf/nelm-source-controller/api/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
	"github.com/deckhouse/operator-helm/internal/common"
	"github.com/deckhouse/operator-helm/internal/utils"
)

const (
	// LabelManagedBy marks resources as managed by this controller.
	LabelManagedBy = "helm.deckhouse.io/managed-by"

	// LabelManagedByValue is the value for the managed-by label.
	LabelManagedByValue = "operator-helm"

	// LabelSourceName stores the name of the source facade resource.
	LabelSourceName = "helm.deckhouse.io/cluster-addon-repository"

	// InternalRepositoryInterval is the interval used in the internal repository spec.
	InternalRepositoryInterval = 5 * time.Minute

	ChartsSyncInterval = 5 * time.Minute

	ConditionTypeReady = "Ready"
)

type RepoService struct {
	BaseService

	TargetNamespace string
}

func NewRepoService(client client.Client, scheme *runtime.Scheme, namespace string) *RepoService {
	return &RepoService{
		BaseService: BaseService{
			Client: client,
			Scheme: scheme,
		},
		TargetNamespace: namespace,
	}
}

type RepoResult struct {
	Status ResourceStatus
}

func (r RepoResult) GetStatus() ResourceStatus {
	return r.Status
}

func (r RepoResult) GetConditionType() string {
	return ConditionTypeReady
}

func (s *RepoService) EnsureInternalHelmRepository(ctx context.Context, repo *helmv1alpha1.HelmClusterAddonRepository) RepoResult {
	logger := log.FromContext(ctx)

	if err := s.reconcileAuthSecret(ctx, repo, utils.InternalHelmRepository); err != nil {
		return RepoResult{Status: Failed(repo, common.ReasonFailed, "Failed to reconcile auth secret", err)}
	}

	if err := s.reconcileTLSSecret(ctx, repo, utils.InternalHelmRepository); err != nil {
		return RepoResult{Status: Failed(repo, common.ReasonFailed, "Failed to reconcile tls secret", err)}
	}

	existing := &sourcev1.HelmRepository{
		ObjectMeta: metav1.ObjectMeta{
			Name:      repo.Name,
			Namespace: s.TargetNamespace,
		},
	}

	op, err := controllerutil.CreateOrPatch(ctx, s.Client, existing, func() error {
		applyInternalHelmRepositorySpec(repo, existing)

		return nil
	})
	if err != nil {
		return RepoResult{
			Status: Failed(
				repo,
				common.ReasonFailed,
				"Failed to reconcile internal helm repository",
				fmt.Errorf("creating internal helm repository: %w", err)),
		}
	}

	if op != controllerutil.OperationResultNone {
		logger.Info("Reconciled internal helm repository", "operation", op)
	}

	if cond, ok := utils.IsConditionObserved(existing.Status.Conditions, ConditionTypeReady, existing.Generation); ok {
		return RepoResult{Status: ResourceStatus{
			Status:             cond.Status,
			ObservedGeneration: repo.Generation,
			Reason:             cond.Reason,
			Message:            cond.Message,
		}}
	}

	return RepoResult{Status: Unknown(repo, ReasonReconciling)}
}

func (s *RepoService) EnsureInternalOCIRepository(ctx context.Context, repo *helmv1alpha1.HelmClusterAddonRepository) RepoResult {
	logger := log.FromContext(ctx)

	if err := s.reconcileAuthSecret(ctx, repo, utils.InternalHelmRepository); err != nil {
		return RepoResult{Status: Failed(repo, common.ReasonFailed, "Failed to reconcile auth secret", err)}
	}

	if err := s.reconcileTLSSecret(ctx, repo, utils.InternalHelmRepository); err != nil {
		return RepoResult{Status: Failed(repo, common.ReasonFailed, "Failed to reconcile tls secret", err)}
	}

	existing := &sourcev1.OCIRepository{
		ObjectMeta: metav1.ObjectMeta{
			Name:      repo.Name,
			Namespace: s.TargetNamespace,
		},
	}

	op, err := controllerutil.CreateOrPatch(ctx, s.Client, existing, func() error {
		applyInternalOCIRepositorySpec(repo, existing)

		return nil
	})
	if err != nil {
		return RepoResult{
			Status: Failed(
				repo,
				common.ReasonFailed,
				"Failed to reconcile internal oci repository",
				fmt.Errorf("creating internal oci repository: %w", err)),
		}
	}

	if op != controllerutil.OperationResultNone {
		logger.Info("Reconciled internal oci repository", "operation", op)
	}

	return RepoResult{Status: Success(repo)}
}

func (s *RepoService) InternalOCIRepositoryCleanup(ctx context.Context, repo *helmv1alpha1.HelmClusterAddonRepository) error {
	resources := []struct {
		name string
		obj  client.Object
	}{
		{
			name: utils.GetInternalRepositoryAuthSecretName(utils.InternalHelmRepository, repo.Name),
			obj:  &corev1.Secret{},
		},
		{
			name: utils.GetInternalRepositoryTLSSecretName(utils.InternalHelmRepository, repo.Name),
			obj:  &corev1.Secret{},
		},
		{
			name: repo.Name,
			obj:  &sourcev1.HelmRepository{},
		},
	}

	for _, r := range resources {
		nn := types.NamespacedName{Name: r.name, Namespace: s.TargetNamespace}
		if err := s.ensureResourceDeleted(ctx, nn, r.obj); err != nil {
			return fmt.Errorf("cleaning up %T %s: %w", r.obj, r.name, err)
		}
	}

	return nil
}

func (s *RepoService) reconcileAuthSecret(ctx context.Context, repo *helmv1alpha1.HelmClusterAddonRepository, repoType utils.InternalRepositoryType) error {
	secretName := utils.GetInternalRepositoryAuthSecretName(repoType, repo.Name)

	if repo.Spec.Auth == nil {
		nn := types.NamespacedName{Name: secretName, Namespace: s.TargetNamespace}
		if err := s.ensureResourceDeleted(ctx, nn, &corev1.Secret{}); err != nil {
			return fmt.Errorf("deleting obsolete auth secret: %w", err)
		}
		return nil
	}

	authSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: s.TargetNamespace,
		},
	}

	if _, err := controllerutil.CreateOrPatch(ctx, s.Client, authSecret, func() error {
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
		return fmt.Errorf("creating auth secret: %w", err)
	}

	return nil
}

func (s *RepoService) reconcileTLSSecret(ctx context.Context, repo *helmv1alpha1.HelmClusterAddonRepository, repoType utils.InternalRepositoryType) error {
	secretName := utils.GetInternalRepositoryTLSSecretName(repoType, repo.Name)

	if repo.Spec.CACertificate == "" {
		nn := types.NamespacedName{Name: secretName, Namespace: s.TargetNamespace}
		if err := s.ensureResourceDeleted(ctx, nn, &corev1.Secret{}); err != nil {
			return fmt.Errorf("deleting obsolete tls secret: %w", err)
		}
		return nil
	}

	// TODO: consider adding CA certificate format validation

	tlsSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: s.TargetNamespace,
		},
	}

	if _, err := controllerutil.CreateOrPatch(ctx, s.Client, tlsSecret, func() error {
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

func applyInternalHelmRepositorySpec(repo *helmv1alpha1.HelmClusterAddonRepository, existing *sourcev1.HelmRepository) {
	existing.Spec.URL = repo.Spec.URL
	existing.Spec.Interval = metav1.Duration{Duration: InternalRepositoryInterval}
	existing.Spec.Insecure = !repo.Spec.TLSVerify
	existing.Spec.CertSecretRef = nil
	existing.Spec.SecretRef = nil

	if repo.Spec.Auth != nil {
		existing.Spec.SecretRef = &meta.LocalObjectReference{
			Name: utils.GetInternalRepositoryAuthSecretName(utils.InternalHelmRepository, repo.Name),
		}
		existing.Spec.PassCredentials = true
	}

	if repo.Spec.CACertificate != "" {
		existing.Spec.CertSecretRef = &meta.LocalObjectReference{
			Name: utils.GetInternalRepositoryTLSSecretName(utils.InternalHelmRepository, repo.Name),
		}
	}

	existing.Labels = map[string]string{
		LabelManagedBy:  LabelManagedByValue,
		LabelSourceName: repo.Name,
	}
}

func applyInternalOCIRepositorySpec(repo *helmv1alpha1.HelmClusterAddonRepository, existing *sourcev1.OCIRepository) {
	existing.Spec.URL = repo.Spec.URL
	existing.Spec.Interval = metav1.Duration{Duration: InternalRepositoryInterval}
	existing.Spec.Insecure = !repo.Spec.TLSVerify
	existing.Spec.CertSecretRef = nil
	existing.Spec.SecretRef = nil

	if repo.Spec.Auth != nil {
		existing.Spec.SecretRef = &meta.LocalObjectReference{
			Name: utils.GetInternalRepositoryAuthSecretName(utils.InternalOCIRepository, repo.Name),
		}
	}

	if repo.Spec.CACertificate != "" {
		existing.Spec.CertSecretRef = &meta.LocalObjectReference{
			Name: utils.GetInternalRepositoryTLSSecretName(utils.InternalOCIRepository, repo.Name),
		}
	}

	existing.Spec.LayerSelector = &sourcev1.OCILayerSelector{
		MediaType: "application/vnd.cncf.helm.chart.content.v1.tar+gzip",
		Operation: "copy",
	}

	existing.Labels = map[string]string{
		LabelManagedBy:  LabelManagedByValue,
		LabelSourceName: repo.Name,
	}
}
