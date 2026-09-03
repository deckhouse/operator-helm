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
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
	_, err := s.deleteAndCheck(ctx, nn, obj)
	return err
}

// deleteAndCheck issues a delete for the object if it is still present and reports
// whether it still exists. A deletion may stay pending because a downstream
// controller (helm-controller, nelm-source-controller) holds a finalizer and has
// not finished tearing the resource down yet, so callers that must not proceed
// until the resource is actually gone should keep requeuing while exists is true.
func (s *BaseService) deleteAndCheck(ctx context.Context, nn types.NamespacedName, obj client.Object) (exists bool, err error) {
	if err := s.Client.Get(ctx, nn, obj); err != nil {
		return false, client.IgnoreNotFound(err)
	}

	if err := s.Client.Delete(ctx, obj); client.IgnoreNotFound(err) != nil {
		return true, fmt.Errorf("failed to delete resource %s/%s: %w", nn.Namespace, nn.Name, err)
	}

	return true, nil
}

type BaseRepoService struct {
	BaseService

	TargetNamespace string
}

// EnsureSecrets reconciles every auxiliary secret the repository needs. Its
// success is the gate for attempting a catalog synchronization: without
// credentials there is nothing to try.
//
// The auth secret's shape depends on the repository kind and the two are not
// interchangeable: HelmRepository resolves HTTP basic auth from an Opaque
// secret, while OCIRepository accepts only a kubernetes.io/dockerconfigjson
// one. An unknown type is treated as helm, mirroring the deletion path — it is
// unreachable here anyway, because an unparsable url is reported before any
// secret is touched.
func (s *BaseRepoService) EnsureSecrets(
	ctx context.Context,
	repo *helmv1alpha1.HelmClusterAddonRepository,
	repoType utils.InternalRepositoryType,
) error {
	var err error

	switch repoType {
	case utils.InternalOCIRepository:
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
func (s *BaseRepoService) reconcileBasicAuthSecret(ctx context.Context, repo *helmv1alpha1.HelmClusterAddonRepository) error {
	return s.reconcileAuthSecret(ctx, repo, corev1.SecretTypeOpaque,
		func(auth *helmv1alpha1.HelmClusterAddonRepositoryAuth) (map[string]string, error) {
			return map[string]string{
				"username": auth.Username,
				"password": auth.Password,
			}, nil
		},
	)
}

// reconcileDockerConfigAuthSecret reconciles the internal auth secret as a
// kubernetes.io/dockerconfigjson secret, the only shape OCIRepository accepts in
// its spec.secretRef.
func (s *BaseRepoService) reconcileDockerConfigAuthSecret(ctx context.Context, repo *helmv1alpha1.HelmClusterAddonRepository) error {
	return s.reconcileAuthSecret(ctx, repo, corev1.SecretTypeDockerConfigJson,
		func(auth *helmv1alpha1.HelmClusterAddonRepositoryAuth) (map[string]string, error) {
			config, err := utils.BuildDockerConfigJSON(repo.Spec.URL, auth.Username, auth.Password)
			if err != nil {
				return nil, fmt.Errorf("building docker config: %w", err)
			}

			return map[string]string{corev1.DockerConfigJsonKey: config}, nil
		},
	)
}

func (s *BaseRepoService) reconcileAuthSecret(
	ctx context.Context,
	repo *helmv1alpha1.HelmClusterAddonRepository,
	secretType corev1.SecretType,
	buildData func(auth *helmv1alpha1.HelmClusterAddonRepositoryAuth) (map[string]string, error),
) error {
	secretName := utils.GetInternalRepositoryAuthSecretName(repo.Name)
	nn := types.NamespacedName{Name: secretName, Namespace: s.TargetNamespace}

	if repo.Spec.Auth == nil {
		if err := s.ensureResourceDeleted(ctx, nn, &corev1.Secret{}); err != nil {
			return fmt.Errorf("deleting obsolete auth secret: %w", err)
		}
		return nil
	}

	stringData, err := buildData(repo.Spec.Auth)
	if err != nil {
		return fmt.Errorf("building auth secret data: %w", err)
	}

	labels := map[string]string{
		helmv1alpha1.LabelManagedBy:                            helmv1alpha1.LabelManagedByValue,
		helmv1alpha1.HelmClusterAddonRepositoryLabelSourceName: repo.Name,
	}

	staleRemoved, err := s.removeAuthSecretOfOtherType(ctx, nn, secretType)
	if err != nil {
		return fmt.Errorf("ensuring auth secret type %q: %w", secretType, err)
	}

	authSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: s.TargetNamespace,
			Labels:    labels,
		},
		Type:       secretType,
		StringData: stringData,
	}

	if staleRemoved {
		// The informer cache can still serve the secret that was just deleted, which
		// would turn CreateOrPatch into a patch of a missing object, so create the
		// replacement outright.
		if err := s.Client.Create(ctx, authSecret); client.IgnoreAlreadyExists(err) != nil {
			return fmt.Errorf("creating auth secret: %w", err)
		}

		return nil
	}

	if _, err := controllerutil.CreateOrPatch(ctx, s.Client, authSecret, func() error {
		authSecret.Labels = labels
		authSecret.Type = secretType
		// Drop the keys already stored so that credentials removed from the desired
		// data do not linger in the secret.
		authSecret.Data = nil
		authSecret.StringData = stringData

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
func (s *BaseRepoService) removeAuthSecretOfOtherType(
	ctx context.Context, nn types.NamespacedName, secretType corev1.SecretType,
) (bool, error) {
	existing := &corev1.Secret{}
	if err := s.Client.Get(ctx, nn, existing); err != nil {
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

	if err := s.Client.Delete(ctx, existing); client.IgnoreNotFound(err) != nil {
		return false, fmt.Errorf("deleting auth secret of type %q: %w", existingType, err)
	}

	return true, nil
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
			helmv1alpha1.LabelManagedBy:                            helmv1alpha1.LabelManagedByValue,
			helmv1alpha1.HelmClusterAddonRepositoryLabelSourceName: repo.Name,
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

// setReconcileRequestAnnotations stamps the flux reconcile/force request
// annotations so the controller owning obj reconciles it immediately instead of
// waiting for its next interval. Both are stamped with the same timestamp:
// ForceRequestAnnotation is only honoured when it matches
// ReconcileRequestAnnotation.
func setReconcileRequestAnnotations(obj metav1.Object) {
	annotations := obj.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}

	ts := time.Now().UTC().Format(time.RFC3339)
	annotations[meta.ForceRequestAnnotation] = ts
	annotations[meta.ReconcileRequestAnnotation] = ts

	obj.SetAnnotations(annotations)
}
