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
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
	"github.com/deckhouse/operator-helm/internal/adapter"
	"github.com/deckhouse/operator-helm/internal/chartsource"
	"github.com/deckhouse/operator-helm/internal/utils"
)

func newSecrets(t *testing.T, objects ...client.Object) (*Secrets, client.Client) {
	t.Helper()

	scheme := testScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()

	return NewSecrets(c, testNamespace), c
}

func TestSecretsEnsureCreatesAuthAndTLS(t *testing.T) {
	repo := &helmv1alpha1.HelmClusterAddonRepository{
		ObjectMeta: metav1.ObjectMeta{Name: "example"},
		Spec: helmv1alpha1.RepositorySpec{
			URL:           "https://example.invalid/charts",
			Auth:          &helmv1alpha1.RepositoryAuth{Username: "user", Password: "secret"},
			CACertificate: "-----BEGIN CERTIFICATE-----",
		},
	}

	service, c := newSecrets(t, repo)

	if err := service.Ensure(context.Background(), adapter.NewAddonRepository(repo), chartsource.Helm); err != nil {
		t.Fatalf("Ensure returned %v", err)
	}

	auth := &corev1.Secret{}
	authKey := types.NamespacedName{Name: utils.GetInternalRepositoryAuthSecretName(repo.Name), Namespace: testNamespace}
	if err := c.Get(context.Background(), authKey, auth); err != nil {
		t.Fatalf("auth secret was not created: %v", err)
	}
	if got := string(auth.Data["username"]); got != "user" {
		t.Fatalf("auth secret username is %q, want %q", got, "user")
	}
	if got := string(auth.Data["password"]); got != "secret" {
		t.Fatalf("auth secret password is %q, want %q", got, "secret")
	}

	tls := &corev1.Secret{}
	tlsKey := types.NamespacedName{Name: utils.GetInternalRepositoryTLSSecretName(repo.Name), Namespace: testNamespace}
	if err := c.Get(context.Background(), tlsKey, tls); err != nil {
		t.Fatalf("tls secret was not created: %v", err)
	}
}

func TestSecretsEnsureRemovesObsoleteSecrets(t *testing.T) {
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

	service, c := newSecrets(t, repo, obsolete)

	if err := service.Ensure(context.Background(), adapter.NewAddonRepository(repo), chartsource.Helm); err != nil {
		t.Fatalf("Ensure returned %v", err)
	}

	err := c.Get(context.Background(), client.ObjectKeyFromObject(obsolete), &corev1.Secret{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("obsolete auth secret must be deleted, got %v", err)
	}
}

func TestSecretsEnsureUsesDockerConfigForOCIRepositories(t *testing.T) {
	repo := &helmv1alpha1.HelmClusterAddonRepository{
		ObjectMeta: metav1.ObjectMeta{Name: "example"},
		Spec: helmv1alpha1.RepositorySpec{
			URL:  "oci://ghcr.io/example/podinfo",
			Auth: &helmv1alpha1.RepositoryAuth{Username: "user", Password: "secret"},
		},
	}

	service, c := newSecrets(t, repo)

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

	config, found := auth.Data[corev1.DockerConfigJsonKey]
	if !found {
		t.Fatalf("auth secret has no %q key, got keys %v", corev1.DockerConfigJsonKey, auth.Data)
	}
	if !strings.Contains(string(config), "ghcr.io") {
		t.Fatalf("docker config does not mention the registry host: %s", config)
	}
}

func TestSecretsCleanupRemovesBothSecrets(t *testing.T) {
	repo := &helmv1alpha1.HelmClusterAddonRepository{
		ObjectMeta: metav1.ObjectMeta{Name: "example"},
		Spec: helmv1alpha1.RepositorySpec{
			URL:           "https://example.invalid/charts",
			Auth:          &helmv1alpha1.RepositoryAuth{Username: "user", Password: "secret"},
			CACertificate: "-----BEGIN CERTIFICATE-----",
		},
	}

	service, c := newSecrets(t, repo)
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

// TestSecretsEnsureIsSilentOnASecondPassWithTheSameInputs pins that a repository
// whose credentials have not changed costs no write at all. The secrets are written
// through CreateOrPatch, which compares the object it read against what the mutation
// left: writing the credentials into StringData made that comparison always differ,
// because the API server folds StringData into Data and never returns it, so every
// reconcile of every repository with credentials sent a patch. The pass that stores
// Data is the only one that can come back equal.
func TestSecretsEnsureIsSilentOnASecondPassWithTheSameInputs(t *testing.T) {
	cases := []struct {
		name     string
		url      string
		repoType chartsource.Kind
	}{
		{name: "helm repository with basic auth", url: "https://example.invalid/charts", repoType: chartsource.Helm},
		{name: "oci repository with a docker config", url: "oci://ghcr.io/example/podinfo", repoType: chartsource.OCI},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := &helmv1alpha1.HelmClusterAddonRepository{
				ObjectMeta: metav1.ObjectMeta{Name: "example"},
				Spec: helmv1alpha1.RepositorySpec{
					URL:           tc.url,
					Auth:          &helmv1alpha1.RepositoryAuth{Username: "user", Password: "secret"},
					CACertificate: "-----BEGIN CERTIFICATE-----",
				},
			}

			var writes []string
			c := fake.NewClientBuilder().
				WithScheme(testScheme(t)).
				WithObjects(repo).
				WithInterceptorFuncs(interceptor.Funcs{
					// The fake client stores what it is given; the API server folds
					// StringData into Data and hands back Data alone. Without that
					// fold the write below could never be seen as redundant, and the
					// test would pass over the very bug it is about.
					Create: func(
						ctx context.Context, wc client.WithWatch, obj client.Object,
						opts ...client.CreateOption,
					) error {
						if secret, ok := obj.(*corev1.Secret); ok {
							foldStringDataIntoData(secret)
						}

						return wc.Create(ctx, obj, opts...)
					},
					Patch: func(
						ctx context.Context, wc client.WithWatch, obj client.Object,
						patch client.Patch, opts ...client.PatchOption,
					) error {
						writes = append(writes, "patch "+obj.GetName())

						return wc.Patch(ctx, obj, patch, opts...)
					},
					Update: func(
						ctx context.Context, wc client.WithWatch, obj client.Object, opts ...client.UpdateOption,
					) error {
						writes = append(writes, "update "+obj.GetName())

						return wc.Update(ctx, obj, opts...)
					},
				}).
				Build()

			service := NewSecrets(c, testNamespace)

			if err := service.Ensure(context.Background(), adapter.NewAddonRepository(repo), tc.repoType); err != nil {
				t.Fatalf("the first Ensure returned %v", err)
			}

			// Only the second pass is under test: the first one creates the secrets,
			// and a create is not counted here.
			writes = nil

			if err := service.Ensure(context.Background(), adapter.NewAddonRepository(repo), tc.repoType); err != nil {
				t.Fatalf("the second Ensure returned %v", err)
			}

			if len(writes) > 0 {
				t.Fatalf("an unchanged repository must write nothing, got %v", writes)
			}
		})
	}
}

// foldStringDataIntoData reproduces what the API server does to a secret on write:
// every StringData entry is stored in Data and StringData is dropped, so a later
// read never sees it.
func foldStringDataIntoData(secret *corev1.Secret) {
	for key, value := range secret.StringData {
		if secret.Data == nil {
			secret.Data = map[string][]byte{}
		}
		secret.Data[key] = []byte(value)
	}

	secret.StringData = nil
}
