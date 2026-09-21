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
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
	"github.com/deckhouse/operator-helm/internal/adapter"
	"github.com/deckhouse/operator-helm/internal/chartsource"
	"github.com/deckhouse/operator-helm/internal/utils"
)

func newRepoSecretsService(t *testing.T, objects ...client.Object) (*RepoSecretsService, client.Client) {
	t.Helper()

	scheme := testScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()

	return NewRepoSecretsService(c, scheme, testNamespace), c
}

func TestEnsureCreatesAuthAndTLS(t *testing.T) {
	repo := &helmv1alpha1.HelmClusterAddonRepository{
		ObjectMeta: metav1.ObjectMeta{Name: "example"},
		Spec: helmv1alpha1.RepositorySpec{
			URL:           "https://example.invalid/charts",
			Auth:          &helmv1alpha1.RepositoryAuth{Username: "user", Password: "secret"},
			CACertificate: "-----BEGIN CERTIFICATE-----",
		},
	}

	service, c := newRepoSecretsService(t, repo)

	if err := service.Ensure(context.Background(), adapter.NewAddonRepository(repo), chartsource.Helm); err != nil {
		t.Fatalf("Ensure returned %v", err)
	}

	auth := &corev1.Secret{}
	authKey := types.NamespacedName{Name: utils.GetInternalRepositoryAuthSecretName(repo.Name), Namespace: testNamespace}
	if err := c.Get(context.Background(), authKey, auth); err != nil {
		t.Fatalf("auth secret was not created: %v", err)
	}
	// The fake client stores what the controller wrote: unlike the API server it
	// does not fold StringData into Data.
	if got := auth.StringData["username"]; got != "user" {
		t.Fatalf("auth secret username is %q, want %q", got, "user")
	}

	tls := &corev1.Secret{}
	tlsKey := types.NamespacedName{Name: utils.GetInternalRepositoryTLSSecretName(repo.Name), Namespace: testNamespace}
	if err := c.Get(context.Background(), tlsKey, tls); err != nil {
		t.Fatalf("tls secret was not created: %v", err)
	}
}

func TestEnsureRemovesObsoleteSecrets(t *testing.T) {
	repo := &helmv1alpha1.HelmClusterAddonRepository{
		ObjectMeta: metav1.ObjectMeta{Name: "example"},
		Spec:       helmv1alpha1.RepositorySpec{URL: "https://example.invalid/charts"},
	}
	obsolete := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      utils.GetInternalRepositoryAuthSecretName(repo.Name),
			Namespace: testNamespace,
		},
	}

	service, c := newRepoSecretsService(t, repo, obsolete)

	if err := service.Ensure(context.Background(), adapter.NewAddonRepository(repo), chartsource.Helm); err != nil {
		t.Fatalf("Ensure returned %v", err)
	}

	err := c.Get(context.Background(), client.ObjectKeyFromObject(obsolete), &corev1.Secret{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("obsolete auth secret must be deleted, got %v", err)
	}
}

func TestEnsureUsesDockerConfigForOCIRepositories(t *testing.T) {
	repo := &helmv1alpha1.HelmClusterAddonRepository{
		ObjectMeta: metav1.ObjectMeta{Name: "example"},
		Spec: helmv1alpha1.RepositorySpec{
			URL:  "oci://ghcr.io/example/podinfo",
			Auth: &helmv1alpha1.RepositoryAuth{Username: "user", Password: "secret"},
		},
	}

	service, c := newRepoSecretsService(t, repo)

	if err := service.Ensure(context.Background(), adapter.NewAddonRepository(repo), chartsource.OCI); err != nil {
		t.Fatalf("Ensure returned %v", err)
	}

	auth := &corev1.Secret{}
	key := types.NamespacedName{Name: utils.GetInternalRepositoryAuthSecretName(repo.Name), Namespace: testNamespace}
	if err := c.Get(context.Background(), key, auth); err != nil {
		t.Fatalf("auth secret was not created: %v", err)
	}

	// OCIRepository resolves credentials only from a dockerconfigjson secret;
	// an Opaque username/password pair is silently ignored by the source controller.
	if auth.Type != corev1.SecretTypeDockerConfigJson {
		t.Fatalf("auth secret type is %q, want %q", auth.Type, corev1.SecretTypeDockerConfigJson)
	}

	config, found := auth.StringData[corev1.DockerConfigJsonKey]
	if !found {
		t.Fatalf("auth secret has no %q key, got keys %v", corev1.DockerConfigJsonKey, auth.StringData)
	}
	if !strings.Contains(config, "ghcr.io") {
		t.Fatalf("docker config does not mention the registry host: %s", config)
	}
}

func TestCleanupRemovesBothSecrets(t *testing.T) {
	repo := &helmv1alpha1.HelmClusterAddonRepository{
		ObjectMeta: metav1.ObjectMeta{Name: "example"},
		Spec: helmv1alpha1.RepositorySpec{
			URL:           "https://example.invalid/charts",
			Auth:          &helmv1alpha1.RepositoryAuth{Username: "user", Password: "secret"},
			CACertificate: "-----BEGIN CERTIFICATE-----",
		},
	}

	service, c := newRepoSecretsService(t, repo)
	names := adapter.NewAddonRepository(repo).InternalNames()

	if err := service.Ensure(context.Background(), adapter.NewAddonRepository(repo), chartsource.Helm); err != nil {
		t.Fatalf("Ensure returned %v", err)
	}

	if err := service.Cleanup(context.Background(), names); err != nil {
		t.Fatalf("Cleanup returned %v", err)
	}

	for _, name := range []string{names.AuthSecret, names.TLSSecret} {
		key := types.NamespacedName{Name: name, Namespace: testNamespace}
		if err := c.Get(context.Background(), key, &corev1.Secret{}); !apierrors.IsNotFound(err) {
			t.Fatalf("secret %s must be deleted, got %v", name, err)
		}
	}
}
