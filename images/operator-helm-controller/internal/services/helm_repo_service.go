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
	apimeta "k8s.io/apimachinery/pkg/api/meta"
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
				fmt.Errorf("creating helm repository: %w", err),
			),
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

// EnsureInternalRepositoryState reconciles the internal HelmRepository and
// reports its observed state. The returned error is an API failure that the
// caller must surface to the work queue; an unhealthy internal object is not an
// error and is reported through the state instead.
func (s *HelmRepoService) EnsureInternalRepositoryState(
	ctx context.Context,
	repo *helmv1alpha1.HelmClusterAddonRepository,
) (InternalRepositoryState, error) {
	logger := log.FromContext(ctx)

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
		return InternalRepositoryState{Present: true}, fmt.Errorf("creating helm repository: %w", err)
	}

	if op != controllerutil.OperationResultNone {
		logger.Info("Reconciled helm repository", "operation", op)
	}

	state := InternalRepositoryState{Present: true}

	if stalled := apimeta.FindStatusCondition(existing.Status.Conditions, helmv1alpha1.ConditionTypeStalled); stalled != nil &&
		stalled.Status == metav1.ConditionTrue {
		state.Stalled = true
		state.Reason = stalled.Reason
		state.Message = stalled.Message

		return state, nil
	}

	cond, observed := status.IsConditionObserved(existing.Status.Conditions, helmv1alpha1.ConditionTypeReady, existing.Generation)
	if !observed {
		state.Reason = helmv1alpha1.ReasonReconciling
		state.Message = "Waiting for the internal repository to be reconciled"

		return state, nil
	}

	state.Ready = cond.Status == metav1.ConditionTrue
	state.Reason = cond.Reason
	state.Message = cond.Message

	return state, nil
}

func (s *HelmRepoService) RemoveHelmRepository(ctx context.Context, repoName string) error {
	name := utils.GetInternalHelmRepositoryName(repoName)
	nn := types.NamespacedName{Name: name, Namespace: s.TargetNamespace}
	if err := s.ensureResourceDeleted(ctx, nn, &sourcev1.HelmRepository{}); err != nil {
		return fmt.Errorf("removing helm repository: %w", err)
	}

	return nil
}

// CleanupHelmRepository removes the auth/TLS secrets (which have no finalizers
// and disappear immediately) and issues a delete for the internal HelmRepository,
// returning it while it is still present so the caller can inspect its conditions
// and wait for nelm-source-controller to finish removing it. It returns nil once
// the HelmRepository is gone.
func (s *HelmRepoService) CleanupHelmRepository(ctx context.Context, repoName string) (*sourcev1.HelmRepository, error) {
	secrets := []string{
		utils.GetInternalRepositoryAuthSecretName(repoName),
		utils.GetInternalRepositoryTLSSecretName(repoName),
	}

	for _, name := range secrets {
		nn := types.NamespacedName{Name: name, Namespace: s.TargetNamespace}
		if err := s.ensureResourceDeleted(ctx, nn, &corev1.Secret{}); err != nil {
			return nil, fmt.Errorf("cleaning up secret %s: %w", name, err)
		}
	}

	nn := types.NamespacedName{Name: utils.GetInternalHelmRepositoryName(repoName), Namespace: s.TargetNamespace}
	helmRepo := &sourcev1.HelmRepository{}
	exists, err := s.deleteAndCheck(ctx, nn, helmRepo)
	if err != nil {
		return nil, fmt.Errorf("cleaning up helm repository: %w", err)
	}
	if !exists {
		return nil, nil
	}

	return helmRepo, nil
}

func applyHelmRepositorySpec(repo *helmv1alpha1.HelmClusterAddonRepository, existing *sourcev1.HelmRepository) {
	if repo.ForceReconcileRequired() {
		if existing.Annotations == nil {
			existing.Annotations = map[string]string{}
		}
		ts := time.Now().UTC().Format(time.RFC3339)
		existing.Annotations[meta.ForceRequestAnnotation] = ts
		existing.Annotations[meta.ReconcileRequestAnnotation] = ts
	}

	existing.Spec.URL = repo.Spec.URL
	existing.Spec.Interval = metav1.Duration{Duration: InternalRepositoryInterval}
	existing.Spec.Insecure = repo.Spec.InsecureSkipVerify
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
