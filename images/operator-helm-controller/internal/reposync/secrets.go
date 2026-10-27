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

package reposync

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
	"github.com/deckhouse/operator-helm/internal/chartsource"
	"github.com/deckhouse/operator-helm/internal/contract"
	"github.com/deckhouse/operator-helm/internal/labels"
	"github.com/deckhouse/operator-helm/internal/utils"
)

// Secrets owns the auth and TLS secrets a repository needs in the target namespace, for
// either repository kind, and removes them again.
type Secrets struct {
	client          client.Client
	reader          client.Reader
	targetNamespace string
}

func NewSecrets(client client.Client, reader client.Reader, namespace string) *Secrets {
	return &Secrets{client: client, reader: reader, targetNamespace: namespace}
}

// Ensure reconciles every auxiliary secret the repository needs. The auth secret's shape
// depends on the repository kind and the two are not interchangeable.
func (s *Secrets) Ensure(
	ctx context.Context,
	repo contract.Repository,
	repoType chartsource.Kind,
) error {
	var err error

	switch repoType {
	case chartsource.OCI:
		err = s.reconcileDockerConfigAuthSecret(ctx, repo)
	default:
		err = s.reconcileBasicAuthSecret(ctx, repo)
	}

	if err != nil {
		return fmt.Errorf("reconciling auth secret: %w", err)
	}

	if err := s.reconcileTLSSecret(ctx, repo); err != nil {
		return fmt.Errorf("reconciling tls secret: %w", err)
	}

	return nil
}

func (s *Secrets) reconcileBasicAuthSecret(ctx context.Context, repo contract.Repository) error {
	// Opaque username/password keys: the shape HelmRepository expects for HTTP basic auth.
	return s.reconcileAuthSecret(ctx, repo, corev1.SecretTypeOpaque,
		func(auth *helmv1alpha1.RepositoryAuth) (map[string][]byte, error) {
			return map[string][]byte{
				"username": []byte(auth.Username),
				"password": []byte(auth.Password),
			}, nil
		},
	)
}

func (s *Secrets) reconcileDockerConfigAuthSecret(ctx context.Context, repo contract.Repository) error {
	// dockerconfigjson: the only shape OCIRepository accepts in its spec.secretRef.
	return s.reconcileAuthSecret(ctx, repo, corev1.SecretTypeDockerConfigJson,
		func(auth *helmv1alpha1.RepositoryAuth) (map[string][]byte, error) {
			config, err := utils.BuildDockerConfigJSON(repo.URL(), auth.Username, auth.Password)
			if err != nil {
				return nil, fmt.Errorf("building docker config: %w", err)
			}

			return map[string][]byte{corev1.DockerConfigJsonKey: []byte(config)}, nil
		},
	)
}

func (s *Secrets) reconcileAuthSecret(
	ctx context.Context,
	repo contract.Repository,
	secretType corev1.SecretType,
	buildData func(auth *helmv1alpha1.RepositoryAuth) (map[string][]byte, error),
) error {
	secretName := repo.InternalNames().AuthSecret
	nn := types.NamespacedName{Name: secretName, Namespace: s.targetNamespace}

	if repo.Auth() == nil {
		if _, err := utils.DeleteIfPresent(ctx, s.client, nn, &corev1.Secret{}); err != nil {
			return fmt.Errorf("deleting obsolete auth secret: %w", err)
		}
		return nil
	}

	// Data, not StringData: the API server never gives StringData back, so every pass would patch.
	data, err := buildData(repo.Auth())
	if err != nil {
		return fmt.Errorf("building auth secret data: %w", err)
	}

	desired := repo.SourceLabels()

	staleRemoved, err := s.removeAuthSecretOfOtherType(ctx, nn, secretType)
	if err != nil {
		return fmt.Errorf("ensuring auth secret type %q: %w", secretType, err)
	}

	authSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: s.targetNamespace,
			Labels:    desired,
		},
		Type: secretType,
		Data: data,
	}

	if staleRemoved {
		// The informer can still serve the deleted secret, turning CreateOrPatch into a patch of a ghost.
		if err := s.client.Create(ctx, authSecret); err != nil {
			return fmt.Errorf("creating auth secret: %w", err)
		}

		return nil
	}

	if _, err := controllerutil.CreateOrPatch(ctx, s.client, authSecret, func() error {
		labels.ApplyOwned(authSecret, desired)
		authSecret.Type = secretType
		// Assigned, not merged, so removed credentials do not linger in the secret.
		authSecret.Data = data

		return nil
	}); err != nil {
		return fmt.Errorf("creating auth secret: %w", err)
	}

	return nil
}

func (s *Secrets) removeAuthSecretOfOtherType(
	ctx context.Context, nn types.NamespacedName, secretType corev1.SecretType,
) (bool, error) {
	// Direct reader, not the cache: a lagging cache would report the old type and cost the
	// secret a needless delete and recreate.
	existing := &corev1.Secret{}
	if err := s.reader.Get(ctx, nn, existing); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}

		return false, fmt.Errorf("getting auth secret: %w", err)
	}

	existingType := existing.Type
	if existingType == "" {
		existingType = corev1.SecretTypeOpaque
	}

	if existingType == secretType {
		return false, nil
	}

	// A secret type is immutable, so a secret of the wrong type can only be replaced.
	if err := s.client.Delete(ctx, existing, client.Preconditions{
		UID:             &existing.UID,
		ResourceVersion: &existing.ResourceVersion,
	}); client.IgnoreNotFound(err) != nil {
		return false, fmt.Errorf("deleting auth secret of type %q: %w", existingType, err)
	}

	return true, nil
}

func (s *Secrets) reconcileTLSSecret(ctx context.Context, repo contract.Repository) error {
	secretName := repo.InternalNames().TLSSecret

	if repo.CACertificate() == "" {
		nn := types.NamespacedName{Name: secretName, Namespace: s.targetNamespace}
		if _, err := utils.DeleteIfPresent(ctx, s.client, nn, &corev1.Secret{}); err != nil {
			return fmt.Errorf("deleting obsolete tls secret: %w", err)
		}
		return nil
	}

	// The certificate is written as given; its format is checked by the repository client that reads with it.
	tlsSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: s.targetNamespace,
		},
	}

	if _, err := controllerutil.CreateOrPatch(ctx, s.client, tlsSecret, func() error {
		labels.ApplyOwned(tlsSecret, repo.SourceLabels())

		// Data, not StringData, for the reason given in reconcileAuthSecret.
		tlsSecret.Data = map[string][]byte{
			"ca.crt": []byte(repo.CACertificate()),
		}

		return nil
	}); err != nil {
		return fmt.Errorf("cannot reconcile tls secret: %w", err)
	}

	return nil
}

// Cleanup removes both secrets. They carry no finalizers, so they disappear at
// once and the caller need not wait for them.
func (s *Secrets) Cleanup(ctx context.Context, names contract.InternalNames) error {
	for _, name := range []string{names.AuthSecret, names.TLSSecret} {
		nn := types.NamespacedName{Name: name, Namespace: s.targetNamespace}
		if _, err := utils.DeleteIfPresent(ctx, s.client, nn, &corev1.Secret{}); err != nil {
			return fmt.Errorf("cleaning up secret %s: %w", name, err)
		}
	}

	return nil
}
