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
	"github.com/deckhouse/operator-helm/internal/manager/status"
	"github.com/deckhouse/operator-helm/internal/utils"
)

const (
	InternalRepositoryInterval = 5 * time.Minute
	ChartsSyncInterval         = 5 * time.Minute
)

type HelmRepoService struct {
	BaseRepoService
}

func NewHelmRepoService(client client.Client, scheme *runtime.Scheme, namespace string) *HelmRepoService {
	return &HelmRepoService{
		BaseRepoService: BaseRepoService{
			BaseService: BaseService{
				Client: client,
				Scheme: scheme,
			},
			TargetNamespace: namespace,
		},
	}
}

var _ status.Provider = (*HelmRepoResult)(nil)

type HelmRepoResult struct {
	Status status.Status
}

func (r HelmRepoResult) GetStatus() status.Status {
	return r.Status
}

func (r HelmRepoResult) IsReady() bool {
	return r.Status.IsReady()
}

func (r HelmRepoResult) GetConditionType() string {
	return helmv1alpha1.ConditionTypeReady
}

func (s *HelmRepoService) EnsureInternalHelmRepository(ctx context.Context, repo *helmv1alpha1.HelmClusterAddonRepository) HelmRepoResult {
	logger := log.FromContext(ctx)

	if err := s.reconcileAuthSecret(ctx, repo); err != nil {
		return HelmRepoResult{Status: status.Failed(repo, helmv1alpha1.ReasonFailed, "Failed to reconcile auth secret", err)}
	}

	if err := s.reconcileTLSSecret(ctx, repo); err != nil {
		return HelmRepoResult{Status: status.Failed(repo, helmv1alpha1.ReasonFailed, "Failed to reconcile tls secret", err)}
	}

	existing := &sourcev1.HelmRepository{
		ObjectMeta: metav1.ObjectMeta{
			Name:      utils.GetInternalHelmRepositoryName(repo.Name),
			Namespace: s.TargetNamespace,
		},
	}

	op, err := controllerutil.CreateOrPatch(ctx, s.Client, existing, func() error {
		applyHelmRepositorySpec(repo, existing)

		return nil
	})
	if err != nil {
		return HelmRepoResult{
			Status: status.Failed(
				repo,
				helmv1alpha1.ReasonFailed,
				"Failed to reconcile helm repository",
				fmt.Errorf("creating helm repository: %w", err)),
		}
	}

	if op != controllerutil.OperationResultNone {
		logger.Info("Reconciled helm repository", "operation", op)
	}

	if cond, ok := status.IsConditionObserved(existing.Status.Conditions, helmv1alpha1.ConditionTypeReady, existing.Generation); ok {
		return HelmRepoResult{Status: status.Status{
			Observed:           ok,
			Status:             cond.Status,
			ObservedGeneration: repo.Generation,
			Reason:             cond.Reason,
			Message:            cond.Message,
		}}
	}

	return HelmRepoResult{Status: status.Unknown(repo, helmv1alpha1.ReasonReconciling)}
}

func (s *HelmRepoService) CleanupHelmRepository(ctx context.Context, repoName string) error {
	resources := []struct {
		name string
		obj  client.Object
	}{
		{
			name: utils.GetInternalRepositoryAuthSecretName(repoName),
			obj:  &corev1.Secret{},
		},
		{
			name: utils.GetInternalRepositoryTLSSecretName(repoName),
			obj:  &corev1.Secret{},
		},
		{
			name: repoName,
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

func applyHelmRepositorySpec(repo *helmv1alpha1.HelmClusterAddonRepository, existing *sourcev1.HelmRepository) {
	existing.Spec.URL = repo.Spec.URL
	existing.Spec.Interval = metav1.Duration{Duration: InternalRepositoryInterval}
	existing.Spec.Insecure = !repo.Spec.TLSVerify
	existing.Spec.CertSecretRef = nil
	existing.Spec.SecretRef = nil

	if repo.Spec.Auth != nil {
		existing.Spec.SecretRef = &meta.LocalObjectReference{
			Name: utils.GetInternalRepositoryAuthSecretName(repo.Name),
		}
		existing.Spec.PassCredentials = true
	}

	if repo.Spec.CACertificate != "" {
		existing.Spec.CertSecretRef = &meta.LocalObjectReference{
			Name: utils.GetInternalRepositoryTLSSecretName(repo.Name),
		}
	}

	existing.Labels = map[string]string{
		helmv1alpha1.LabelManagedBy:                            helmv1alpha1.LabelManagedByValue,
		helmv1alpha1.HelmClusterAddonRepositoryLabelSourceName: repo.Name,
	}
}
