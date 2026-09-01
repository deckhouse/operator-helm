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
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
	"github.com/deckhouse/operator-helm/internal/utils"
)

const testNamespace = "d8-operator-helm"

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("registering client-go scheme: %v", err)
	}
	if err := helmv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("registering helm scheme: %v", err)
	}

	return scheme
}

func newBaseRepoService(t *testing.T, objects ...client.Object) (*BaseRepoService, client.Client) {
	t.Helper()

	scheme := testScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()

	return &BaseRepoService{
		BaseService:     BaseService{Client: c, Scheme: scheme},
		TargetNamespace: testNamespace,
	}, c
}

func TestEnsureSecretsCreatesAuthAndTLS(t *testing.T) {
	repo := &helmv1alpha1.HelmClusterAddonRepository{
		ObjectMeta: metav1.ObjectMeta{Name: "example"},
		Spec: helmv1alpha1.HelmClusterAddonRepositorySpec{
			URL:           "https://example.invalid/charts",
			Auth:          &helmv1alpha1.HelmClusterAddonRepositoryAuth{Username: "user", Password: "secret"},
			CACertificate: "-----BEGIN CERTIFICATE-----",
		},
	}

	service, c := newBaseRepoService(t, repo)

	if err := service.EnsureSecrets(context.Background(), repo); err != nil {
		t.Fatalf("EnsureSecrets returned %v", err)
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

func TestEnsureSecretsRemovesObsoleteSecrets(t *testing.T) {
	repo := &helmv1alpha1.HelmClusterAddonRepository{
		ObjectMeta: metav1.ObjectMeta{Name: "example"},
		Spec:       helmv1alpha1.HelmClusterAddonRepositorySpec{URL: "https://example.invalid/charts"},
	}
	obsolete := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      utils.GetInternalRepositoryAuthSecretName(repo.Name),
			Namespace: testNamespace,
		},
	}

	service, c := newBaseRepoService(t, repo, obsolete)

	if err := service.EnsureSecrets(context.Background(), repo); err != nil {
		t.Fatalf("EnsureSecrets returned %v", err)
	}

	err := c.Get(context.Background(), client.ObjectKeyFromObject(obsolete), &corev1.Secret{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("obsolete auth secret must be deleted, got %v", err)
	}
}
