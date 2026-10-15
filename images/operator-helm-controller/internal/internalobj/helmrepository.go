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

package internalobj

import (
	"context"
	"fmt"
	"time"

	"github.com/fluxcd/pkg/apis/meta"
	sourcev1 "github.com/fluxcd/source-controller/api/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
	"github.com/deckhouse/operator-helm/internal/contract"
	"github.com/deckhouse/operator-helm/internal/utils"
)

const InternalRepositoryInterval = 5 * time.Minute

type HelmRepositories struct {
	client          client.Client
	targetNamespace string
}

func NewHelmRepositories(client client.Client, namespace string) *HelmRepositories {
	return &HelmRepositories{client: client, targetNamespace: namespace}
}

// Ensure reconciles the internal HelmRepository and reports its observed state. The error
// is an API failure; an unhealthy internal object is reported through the state instead.
func (s *HelmRepositories) Ensure(
	ctx context.Context,
	repo contract.Repository,
) (InternalRepositoryState, error) {
	logger := log.FromContext(ctx)

	existing := &sourcev1.HelmRepository{
		ObjectMeta: metav1.ObjectMeta{
			Name:      repo.InternalNames().HelmRepository,
			Namespace: s.targetNamespace,
		},
	}

	op, err := controllerutil.CreateOrPatch(ctx, s.client, existing, func() error {
		applyHelmRepositorySpec(repo, existing)

		return nil
	})
	if err != nil {
		return InternalRepositoryState{Present: true}, fmt.Errorf("creating helm repository: %w", err)
	}

	if op != controllerutil.OperationResultNone {
		logger.Info("Reconciled helm repository", "operation", op,
			"internalObject", client.ObjectKeyFromObject(existing))
	}

	state := InternalRepositoryState{Present: true}

	// The Stalled condition counts only while it speaks about the generation the object now has.
	if stalled := currentCondition(existing.Status.Conditions, helmv1alpha1.ConditionTypeStalled, existing.Generation); stalled != nil &&
		stalled.Status == metav1.ConditionTrue {
		state.Stalled = true
		state.Reason = stalled.Reason
		state.Message = stalled.Message

		return state, nil
	}

	cond, observed := conditionObserved(existing.Status.Conditions, helmv1alpha1.ConditionTypeReady, existing.Generation)
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

func (s *HelmRepositories) Remove(ctx context.Context, names contract.InternalNames) error {
	nn := types.NamespacedName{Name: names.HelmRepository, Namespace: s.targetNamespace}
	if _, err := utils.DeleteIfPresent(ctx, s.client, nn, &sourcev1.HelmRepository{}); err != nil {
		return fmt.Errorf("removing helm repository: %w", err)
	}

	return nil
}

// Cleanup issues a delete for the internal HelmRepository and returns it while it is
// still present, so the caller can wait for source-controller to finish removing it.
func (s *HelmRepositories) Cleanup(ctx context.Context, names contract.InternalNames) (*sourcev1.HelmRepository, error) {
	nn := types.NamespacedName{Name: names.HelmRepository, Namespace: s.targetNamespace}
	helmRepo := &sourcev1.HelmRepository{}
	exists, err := utils.DeleteIfPresent(ctx, s.client, nn, helmRepo)
	if err != nil {
		return nil, fmt.Errorf("cleaning up helm repository: %w", err)
	}
	if !exists {
		return nil, nil
	}

	return helmRepo, nil
}

func applyHelmRepositorySpec(repo contract.Repository, existing *sourcev1.HelmRepository) {
	if repo.ForceReconcileRequired() {
		setReconcileRequestAnnotations(existing)
	}

	names := repo.InternalNames()

	existing.Spec.URL = repo.URL()
	existing.Spec.Interval = metav1.Duration{Duration: InternalRepositoryInterval}
	existing.Spec.Insecure = repo.InsecureSkipVerify()
	existing.Spec.CertSecretRef = nil
	existing.Spec.SecretRef = nil
	// Reset with the reference it belongs to: this must not outlive the secret it was set for.
	existing.Spec.PassCredentials = false

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

	applyOwnedLabels(existing, repo.SourceLabels())
}
