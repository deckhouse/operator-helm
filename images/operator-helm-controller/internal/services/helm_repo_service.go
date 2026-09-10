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
	"github.com/deckhouse/operator-helm/internal/source"
)

const InternalRepositoryInterval = 5 * time.Minute

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

// EnsureInternalHelmRepository reconciles the internal HelmRepository and
// reports its observed state. The returned error is an API failure that the
// caller must surface to the work queue; an unhealthy internal object is not an
// error and is reported through the state instead.
func (s *HelmRepoService) EnsureInternalHelmRepository(
	ctx context.Context,
	repo source.Repository,
) (InternalRepositoryState, error) {
	logger := log.FromContext(ctx)

	existing := &sourcev1.HelmRepository{
		ObjectMeta: metav1.ObjectMeta{
			Name:      repo.InternalNames().HelmRepository,
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

func (s *HelmRepoService) RemoveHelmRepository(ctx context.Context, names source.InternalNames) error {
	nn := types.NamespacedName{Name: names.HelmRepository, Namespace: s.TargetNamespace}
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
func (s *HelmRepoService) CleanupHelmRepository(ctx context.Context, names source.InternalNames) (*sourcev1.HelmRepository, error) {
	for _, name := range []string{names.AuthSecret, names.TLSSecret} {
		nn := types.NamespacedName{Name: name, Namespace: s.TargetNamespace}
		if err := s.ensureResourceDeleted(ctx, nn, &corev1.Secret{}); err != nil {
			return nil, fmt.Errorf("cleaning up secret %s: %w", name, err)
		}
	}

	nn := types.NamespacedName{Name: names.HelmRepository, Namespace: s.TargetNamespace}
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

func applyHelmRepositorySpec(repo source.Repository, existing *sourcev1.HelmRepository) {
	if repo.ForceReconcileRequired() {
		if existing.Annotations == nil {
			existing.Annotations = map[string]string{}
		}
		ts := time.Now().UTC().Format(time.RFC3339)
		existing.Annotations[meta.ForceRequestAnnotation] = ts
		existing.Annotations[meta.ReconcileRequestAnnotation] = ts
	}

	names := repo.InternalNames()

	existing.Spec.URL = repo.URL()
	existing.Spec.Interval = metav1.Duration{Duration: InternalRepositoryInterval}
	existing.Spec.Insecure = repo.InsecureSkipVerify()
	existing.Spec.CertSecretRef = nil
	existing.Spec.SecretRef = nil

	if repo.Auth() != nil {
		existing.Spec.SecretRef = &meta.LocalObjectReference{
			Name: names.AuthSecret,
		}
		existing.Spec.PassCredentials = true
	}

	if repo.CACertificate() != "" {
		existing.Spec.CertSecretRef = &meta.LocalObjectReference{
			Name: names.TLSSecret,
		}
	}

	existing.Labels = repo.SourceLabels()
}
