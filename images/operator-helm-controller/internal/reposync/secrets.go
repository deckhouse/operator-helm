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
	"github.com/deckhouse/operator-helm/internal/utils"
)

// Secrets owns the auth and TLS secrets a repository needs in the
// target namespace, for either repository kind. The secrets outlive neither the
// repository nor its internal source object, so the service that writes them also
// removes them, and the repository services are left with the objects they own.
type Secrets struct {
	client          client.Client
	targetNamespace string
}

func NewSecrets(client client.Client, namespace string) *Secrets {
	return &Secrets{client: client, targetNamespace: namespace}
}

// Ensure reconciles every auxiliary secret the repository needs. Its success is
// the gate for attempting a catalog synchronization: without credentials there is
// nothing to try.
//
// The auth secret's shape depends on the repository kind and the two are not
// interchangeable: HelmRepository resolves HTTP basic auth from an Opaque
// secret, while OCIRepository accepts only a kubernetes.io/dockerconfigjson
// one. An unknown type is treated as helm, mirroring the deletion path — it is
// unreachable here anyway, because an unparsable url is reported before any
// secret is touched.
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

// reconcileBasicAuthSecret reconciles the internal auth secret as an Opaque secret
// holding username/password keys, the shape HelmRepository expects for HTTP basic
// auth.
func (s *Secrets) reconcileBasicAuthSecret(ctx context.Context, repo contract.Repository) error {
	return s.reconcileAuthSecret(ctx, repo, corev1.SecretTypeOpaque,
		func(auth *helmv1alpha1.RepositoryAuth) (map[string][]byte, error) {
			return map[string][]byte{
				"username": []byte(auth.Username),
				"password": []byte(auth.Password),
			}, nil
		},
	)
}

// reconcileDockerConfigAuthSecret reconciles the internal auth secret as a
// kubernetes.io/dockerconfigjson secret, the only shape OCIRepository accepts in
// its spec.secretRef.
func (s *Secrets) reconcileDockerConfigAuthSecret(ctx context.Context, repo contract.Repository) error {
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

	// The data is built in its stored form rather than as StringData: the API server
	// folds StringData into Data and never gives it back, so a mutation writing it
	// would differ from what was read on every pass and send a patch on every
	// reconcile of every repository with credentials.
	data, err := buildData(repo.Auth())
	if err != nil {
		return fmt.Errorf("building auth secret data: %w", err)
	}

	labels := repo.SourceLabels()

	staleRemoved, err := s.removeAuthSecretOfOtherType(ctx, nn, secretType)
	if err != nil {
		return fmt.Errorf("ensuring auth secret type %q: %w", secretType, err)
	}

	authSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: s.targetNamespace,
			Labels:    labels,
		},
		Type: secretType,
		Data: data,
	}

	if staleRemoved {
		// The informer cache can still serve the secret that was just deleted, which
		// would turn CreateOrPatch into a patch of a missing object, so create the
		// replacement outright.
		if err := s.client.Create(ctx, authSecret); client.IgnoreAlreadyExists(err) != nil {
			return fmt.Errorf("creating auth secret: %w", err)
		}

		return nil
	}

	if _, err := controllerutil.CreateOrPatch(ctx, s.client, authSecret, func() error {
		authSecret.Labels = labels
		authSecret.Type = secretType
		// Assigned rather than merged, so that credentials removed from the desired
		// data do not linger in the secret.
		authSecret.Data = data

		return nil
	}); err != nil {
		return fmt.Errorf("creating auth secret: %w", err)
	}

	return nil
}

// removeAuthSecretOfOtherType deletes the auth secret when it exists with a type
// other than the wanted one and reports whether it did. A secret type is immutable,
// so a secret written with the wrong type (an Opaque one left by a version that fed
// plain credentials to OCIRepository, say) can only be replaced, not patched.
func (s *Secrets) removeAuthSecretOfOtherType(
	ctx context.Context, nn types.NamespacedName, secretType corev1.SecretType,
) (bool, error) {
	existing := &corev1.Secret{}
	if err := s.client.Get(ctx, nn, existing); err != nil {
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

	if err := s.client.Delete(ctx, existing); client.IgnoreNotFound(err) != nil {
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

	// The certificate is written as given. Its format is checked where it is used,
	// by the repository client building the trust root for a read: a failure there is
	// terminal and names the field, while a failure here would be an auxiliary
	// resource failure and retried forever.
	tlsSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: s.targetNamespace,
		},
	}

	if _, err := controllerutil.CreateOrPatch(ctx, s.client, tlsSecret, func() error {
		tlsSecret.Labels = repo.SourceLabels()

		// Data, not StringData, for the reason given in reconcileAuthSecret: only Data
		// comes back from a read, so only Data can compare equal to what is stored.
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
