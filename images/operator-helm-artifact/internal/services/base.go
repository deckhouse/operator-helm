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

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
	"github.com/deckhouse/operator-helm/internal/utils"
)

type BaseService struct {
	Client client.Client
	Scheme *runtime.Scheme
}

func (s *BaseService) ensureResourceDeleted(ctx context.Context, nn types.NamespacedName, obj client.Object) error {
	err := s.Client.Get(ctx, nn, obj)
	if err != nil {
		return client.IgnoreNotFound(err)
	}

	if err := s.Client.Delete(ctx, obj); client.IgnoreNotFound(err) != nil {
		return fmt.Errorf("failed to delete resource %s/%s: %w", nn.Namespace, nn.Name, err)
	}

	return nil
}

type BaseRepoService struct {
	BaseService

	TargetNamespace string
}

func (s *BaseRepoService) reconcileAuthSecret(ctx context.Context, repo *helmv1alpha1.HelmClusterAddonRepository) error {
	secretName := utils.GetInternalRepositoryAuthSecretName(repo.Name)

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
			helmv1alpha1.LabelManagedBy:                            helmv1alpha1.LabelManagedByValue,
			helmv1alpha1.HelmClusterAddonRepositoryLabelSourceName: repo.Name,
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

func (s *BaseRepoService) reconcileTLSSecret(ctx context.Context, repo *helmv1alpha1.HelmClusterAddonRepository) error {
	secretName := utils.GetInternalRepositoryTLSSecretName(repo.Name)

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
			helmv1alpha1.LabelManagedBy:                  helmv1alpha1.LabelManagedByValue,
			helmv1alpha1.HelmClusterAddonLabelSourceName: repo.Name,
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
