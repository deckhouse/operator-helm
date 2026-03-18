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

type OCIRepoService struct {
	BaseRepoService
}

func NewOCIRepoService(client client.Client, scheme *runtime.Scheme, namespace string) *OCIRepoService {
	return &OCIRepoService{
		BaseRepoService: BaseRepoService{
			BaseService: BaseService{
				Client: client,
				Scheme: scheme,
			},
			TargetNamespace: namespace,
		},
	}
}

var _ status.Provider = (*OCIRepoResult)(nil)

type OCIRepoResult struct {
	Status   status.Status
	Artifact *meta.Artifact
}

func (r OCIRepoResult) GetStatus() status.Status {
	return r.Status
}

func (r OCIRepoResult) IsReady() bool {
	return r.Artifact != nil && r.Status.Observed && r.Status.Status == metav1.ConditionTrue
}

func (r OCIRepoResult) IsPartiallyDegraded() bool {
	return r.Artifact != nil && r.Status.Status != metav1.ConditionTrue && r.Status.Observed
}

func (r OCIRepoResult) HasArtifact() bool {
	return r.Artifact != nil && r.Status.Observed
}

func (r OCIRepoResult) GetConditionType() string {
	if r.Status.ConditionType == "" {
		return helmv1alpha1.ConditionTypePartiallyDegraded
	}
	return r.Status.ConditionType
}

func (s *OCIRepoService) EnsureInternalOCIRepository(ctx context.Context, addon *helmv1alpha1.HelmClusterAddon, repo *helmv1alpha1.HelmClusterAddonRepository) OCIRepoResult {
	logger := log.FromContext(ctx)

	existing := &sourcev1.OCIRepository{
		ObjectMeta: metav1.ObjectMeta{
			Name:      utils.GetInternalOCIRepositoryName(addon.Name),
			Namespace: s.TargetNamespace,
		},
	}

	op, err := controllerutil.CreateOrPatch(ctx, s.Client, existing, func() error {
		applyOCIRepositorySpec(addon, repo, existing)

		return nil
	})
	if err != nil {
		return OCIRepoResult{
			Status: status.Failed(
				addon,
				helmv1alpha1.ReasonFailed,
				"Failed to reconcile oci repository",
				fmt.Errorf("creating oci repository: %w", err)),
		}
	}

	if op != controllerutil.OperationResultNone {
		logger.Info("Reconciled oci repository", "operation", op)
	}

	if cond, ok := status.IsConditionObserved(existing.Status.Conditions, helmv1alpha1.ConditionTypeReady, existing.Generation); ok {
		return OCIRepoResult{
			Artifact: existing.Status.Artifact,
			Status: status.Status{
				Observed:           ok,
				Status:             cond.Status,
				ObservedGeneration: addon.Generation,
				Reason:             cond.Reason,
				Message:            cond.Message,
				NotReflectable:     existing.Status.Artifact != nil,
			},
		}
	}

	return OCIRepoResult{Status: status.Unknown(addon, helmv1alpha1.ReasonReconciling)}
}

func (s *OCIRepoService) EnsureRepositorySecrets(ctx context.Context, repo *helmv1alpha1.HelmClusterAddonRepository) OCIRepoResult {
	if err := s.reconcileAuthSecret(ctx, repo); err != nil {
		return OCIRepoResult{
			Status: status.Status{
				ConditionType:      helmv1alpha1.ConditionTypeReady,
				Observed:           true,
				Status:             metav1.ConditionFalse,
				ObservedGeneration: repo.Generation,
				Reason:             helmv1alpha1.ReasonFailed,
				Message:            "Failed to reconcile auth secret",
				Err:                err,
			},
		}
	}

	if err := s.reconcileTLSSecret(ctx, repo); err != nil {
		return OCIRepoResult{
			Status: status.Status{
				ConditionType:      helmv1alpha1.ConditionTypeReady,
				Observed:           true,
				Status:             metav1.ConditionFalse,
				ObservedGeneration: repo.Generation,
				Reason:             helmv1alpha1.ReasonFailed,
				Message:            "Failed to reconcile tls secret",
				Err:                err,
			},
		}
	}

	return OCIRepoResult{
		Artifact: &meta.Artifact{},
		Status: status.Status{
			ConditionType:      helmv1alpha1.ConditionTypeReady,
			Observed:           true,
			Status:             metav1.ConditionTrue,
			ObservedGeneration: repo.Generation,
			Reason:             helmv1alpha1.ReasonSuccess,
		},
	}
}

func (s *OCIRepoService) CleanupOCIRepository(ctx context.Context, repoName string) error {
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
	}

	for _, r := range resources {
		nn := types.NamespacedName{Name: r.name, Namespace: s.TargetNamespace}
		if err := s.ensureResourceDeleted(ctx, nn, r.obj); err != nil {
			return fmt.Errorf("cleaning up %T %s: %w", r.obj, r.name, err)
		}
	}

	return nil
}

func (s *OCIRepoService) RemoveOCIRepository(ctx context.Context, addon *helmv1alpha1.HelmClusterAddon) error {
	name := utils.GetInternalOCIRepositoryName(addon.Name)
	nn := types.NamespacedName{Name: name, Namespace: s.TargetNamespace}
	if err := s.ensureResourceDeleted(ctx, nn, &sourcev1.OCIRepository{}); err != nil {
		return fmt.Errorf("removing oci repository: %w", err)
	}

	return nil
}

func applyOCIRepositorySpec(addon *helmv1alpha1.HelmClusterAddon, repo *helmv1alpha1.HelmClusterAddonRepository, existing *sourcev1.OCIRepository) {
	existing.Spec.URL = repo.Spec.URL
	existing.Spec.Reference = &sourcev1.OCIRepositoryRef{
		Tag: addon.Spec.Chart.Version,
	}
	existing.Spec.Interval = metav1.Duration{Duration: InternalRepositoryInterval}
	existing.Spec.Insecure = !repo.Spec.TLSVerify
	existing.Spec.CertSecretRef = nil
	existing.Spec.SecretRef = nil

	if repo.Spec.Auth != nil {
		existing.Spec.SecretRef = &meta.LocalObjectReference{
			Name: utils.GetInternalRepositoryAuthSecretName(repo.Name),
		}
	}

	if repo.Spec.CACertificate != "" {
		existing.Spec.CertSecretRef = &meta.LocalObjectReference{
			Name: utils.GetInternalRepositoryTLSSecretName(repo.Name),
		}
	}

	existing.Spec.LayerSelector = &sourcev1.OCILayerSelector{
		MediaType: "application/vnd.cncf.helm.chart.content.v1.tar+gzip",
		Operation: "copy",
	}

	existing.Labels = map[string]string{
		helmv1alpha1.LabelManagedBy:                  helmv1alpha1.LabelManagedByValue,
		helmv1alpha1.HelmClusterAddonLabelSourceName: addon.Name,
	}
}
