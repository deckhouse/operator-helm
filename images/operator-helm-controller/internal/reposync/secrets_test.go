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
	"maps"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
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

	return NewSecrets(c, c, testNamespace), c
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

			service := NewSecrets(c, c, testNamespace)

			if err := service.Ensure(context.Background(), adapter.NewAddonRepository(repo), tc.repoType); err != nil {
				t.Fatalf("the first Ensure returned %v", err)
			}

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

func foldStringDataIntoData(secret *corev1.Secret) {
	for key, value := range secret.StringData {
		if secret.Data == nil {
			secret.Data = map[string][]byte{}
		}
		secret.Data[key] = []byte(value)
	}

	secret.StringData = nil
}

func TestSecretsEnsureFailsWhenTheReplacedAuthSecretIsBack(t *testing.T) {
	repo := &helmv1alpha1.HelmClusterAddonRepository{
		ObjectMeta: metav1.ObjectMeta{Name: "example"},
		Spec: helmv1alpha1.RepositorySpec{
			URL:  "oci://ghcr.io/example/podinfo",
			Auth: &helmv1alpha1.RepositoryAuth{Username: "user", Password: "secret"},
		},
	}

	stale := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      utils.GetInternalRepositoryAuthSecretName(repo.Name),
			Namespace: testNamespace,
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{"username": []byte("stale")},
	}

	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(repo, stale).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(
				ctx context.Context, wc client.WithWatch, obj client.Object,
				opts ...client.CreateOption,
			) error {
				return apierrors.NewAlreadyExists(schema.GroupResource{Resource: "secrets"}, obj.GetName())
			},
		}).
		Build()

	service := NewSecrets(c, c, testNamespace)

	err := service.Ensure(context.Background(), adapter.NewAddonRepository(repo), chartsource.OCI)
	if err == nil {
		t.Fatal("Ensure reported success without ever writing the auth secret data")
	}
	if !apierrors.IsAlreadyExists(err) {
		t.Fatalf("Ensure returned %v, want the AlreadyExists that stopped the write", err)
	}
}

func TestSecretsEnsureKeepsLabelsSetBySomeoneElse(t *testing.T) {
	repo := &helmv1alpha1.HelmClusterAddonRepository{
		ObjectMeta: metav1.ObjectMeta{Name: "example"},
		Spec: helmv1alpha1.RepositorySpec{
			URL:           "https://example.invalid/charts",
			Auth:          &helmv1alpha1.RepositoryAuth{Username: "user", Password: "secret"},
			CACertificate: "-----BEGIN CERTIFICATE-----",
		},
	}

	names := adapter.NewAddonRepository(repo).InternalNames()

	foreign := map[string]string{"cost-center": "team-a", "policy.example.com/tier": "gold"}
	existing := []client.Object{repo}

	for _, name := range []string{names.AuthSecret, names.TLSSecret} {
		existing = append(existing, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: testNamespace,
				Labels:    maps.Clone(foreign),
			},
			Type: corev1.SecretTypeOpaque,
		})
	}

	service, c := newSecrets(t, existing...)

	if err := service.Ensure(context.Background(), adapter.NewAddonRepository(repo), chartsource.Helm); err != nil {
		t.Fatalf("Ensure returned %v", err)
	}

	for _, name := range []string{names.AuthSecret, names.TLSSecret} {
		secret := &corev1.Secret{}
		if err := c.Get(context.Background(), types.NamespacedName{Name: name, Namespace: testNamespace}, secret); err != nil {
			t.Fatalf("getting secret %s: %v", name, err)
		}

		for key, value := range foreign {
			if got, found := secret.Labels[key]; !found || got != value {
				t.Fatalf("secret %s lost the label %q set by someone else, labels are %v", name, key, secret.Labels)
			}
		}

		if secret.Labels[helmv1alpha1.LabelManagedBy] != helmv1alpha1.LabelManagedByValue {
			t.Fatalf("secret %s is missing the managed-by label, labels are %v", name, secret.Labels)
		}
	}
}

func TestSecretsEnsureKeepsAnAuthSecretTheCacheReportsWithAnOldType(t *testing.T) {
	repo := &helmv1alpha1.HelmClusterAddonRepository{
		ObjectMeta: metav1.ObjectMeta{Name: "example"},
		Spec: helmv1alpha1.RepositorySpec{
			URL:  "oci://ghcr.io/example/podinfo",
			Auth: &helmv1alpha1.RepositoryAuth{Username: "user", Password: "secret"},
		},
	}

	authName := utils.GetInternalRepositoryAuthSecretName(repo.Name)
	current := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: authName, Namespace: testNamespace},
		Type:       corev1.SecretTypeDockerConfigJson,
		Data:       map[string][]byte{corev1.DockerConfigJsonKey: []byte("{}")},
	}

	fresh := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(repo, current).Build()

	var deletes []string
	cached := interceptor.NewClient(fresh, interceptor.Funcs{
		Get: func(
			ctx context.Context, wc client.WithWatch, key client.ObjectKey,
			obj client.Object, opts ...client.GetOption,
		) error {
			if err := wc.Get(ctx, key, obj, opts...); err != nil {
				return err
			}

			if secret, ok := obj.(*corev1.Secret); ok && secret.Name == authName {
				secret.Type = corev1.SecretTypeOpaque
			}

			return nil
		},
		Delete: func(
			ctx context.Context, wc client.WithWatch, obj client.Object, opts ...client.DeleteOption,
		) error {
			deletes = append(deletes, obj.GetName())

			return wc.Delete(ctx, obj, opts...)
		},
	})

	service := NewSecrets(cached, fresh, testNamespace)

	if err := service.Ensure(context.Background(), adapter.NewAddonRepository(repo), chartsource.OCI); err != nil {
		t.Fatalf("Ensure returned %v", err)
	}

	if len(deletes) > 0 {
		t.Fatalf("an auth secret of the right type must survive a lagging cache, deleted %v", deletes)
	}
}

func TestSecretsEnsureDoesNotDeleteAnAuthSecretThatChangedSinceItWasRead(t *testing.T) {
	repo := &helmv1alpha1.HelmClusterAddonRepository{
		ObjectMeta: metav1.ObjectMeta{Name: "example"},
		Spec: helmv1alpha1.RepositorySpec{
			URL:  "oci://ghcr.io/example/podinfo",
			Auth: &helmv1alpha1.RepositoryAuth{Username: "user", Password: "secret"},
		},
	}

	authName := utils.GetInternalRepositoryAuthSecretName(repo.Name)
	stale := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: authName, Namespace: testNamespace},
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{"username": []byte("stale")},
	}

	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(repo, stale).Build()

	raced := false
	cached := interceptor.NewClient(c, interceptor.Funcs{
		Delete: func(
			ctx context.Context, wc client.WithWatch, obj client.Object, opts ...client.DeleteOption,
		) error {
			if !raced {
				raced = true

				replaced := &corev1.Secret{}
				if err := wc.Get(ctx, client.ObjectKeyFromObject(obj), replaced); err != nil {
					return err
				}
				if err := wc.Delete(ctx, replaced); err != nil {
					return err
				}
				if err := wc.Create(ctx, &corev1.Secret{
					ObjectMeta: metav1.ObjectMeta{Name: authName, Namespace: testNamespace},
					Type:       corev1.SecretTypeDockerConfigJson,
					Data:       map[string][]byte{corev1.DockerConfigJsonKey: []byte("{}")},
				}); err != nil {
					return err
				}
			}

			return wc.Delete(ctx, obj, opts...)
		},
	})

	service := NewSecrets(cached, c, testNamespace)

	if err := service.Ensure(context.Background(), adapter.NewAddonRepository(repo), chartsource.OCI); err == nil {
		t.Fatal("Ensure deleted an auth secret that had already been replaced")
	}

	secret := &corev1.Secret{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: authName, Namespace: testNamespace}, secret); err != nil {
		t.Fatalf("the replacing auth secret is gone: %v", err)
	}
	if secret.Type != corev1.SecretTypeDockerConfigJson {
		t.Fatalf("auth secret type is %q, want %q", secret.Type, corev1.SecretTypeDockerConfigJson)
	}
}
