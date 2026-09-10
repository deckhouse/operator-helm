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
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
	"github.com/deckhouse/operator-helm/internal/source"
)

// ApplicationRoleName is the Role every application release of a namespace is bound
// to. One Role per namespace: cutting it down once applies to every application
// there, and the edit survives applications being recreated.
const ApplicationRoleName = "operator-helm-application"

var _ source.AccessManager = (*AccessService)(nil)

// AccessService provides the identity a release is applied with: a ServiceAccount in
// the operator namespace (next to the HelmRelease, where helm-controller looks it
// up), a Role in the target namespace and a RoleBinding tying the two together.
//
// The Role is bound through a RoleBinding, so whatever it grants stops at the
// namespace boundary: cluster-scoped resources are unreachable regardless of its
// content. It is also the one object a namespace owner may edit to cut the rights
// down — which is why it is created once and never reconciled afterwards.
type AccessService struct {
	BaseService

	TargetNamespace string
}

func NewAccessService(c client.Client, targetNamespace string) *AccessService {
	return &AccessService{
		BaseService:     BaseService{Client: c},
		TargetNamespace: targetNamespace,
	}
}

// EnsureAccess reconciles the account and the binding and seeds the Role. A release
// without a service account name belongs to a family that does not impersonate;
// nothing is created for it.
func (s *AccessService) EnsureAccess(ctx context.Context, rel source.Release) error {
	name := rel.InternalNames().ServiceAccount
	if name == "" {
		return nil
	}

	if err := s.ensureServiceAccount(ctx, rel, name); err != nil {
		return fmt.Errorf("ensuring service account: %w", err)
	}

	if err := s.seedRole(ctx, rel.TargetNamespace()); err != nil {
		return fmt.Errorf("seeding role: %w", err)
	}

	if err := s.ensureRoleBinding(ctx, rel, name); err != nil {
		return fmt.Errorf("ensuring role binding: %w", err)
	}

	return nil
}

// CleanupAccess removes the account and the binding. The Role stays: it belongs to
// the namespace, may carry the owner's edits, and grants nothing without a binding.
func (s *AccessService) CleanupAccess(ctx context.Context, rel source.Release) error {
	name := rel.InternalNames().ServiceAccount
	if name == "" {
		return nil
	}

	binding := types.NamespacedName{Namespace: rel.TargetNamespace(), Name: name}
	if err := s.ensureResourceDeleted(ctx, binding, &rbacv1.RoleBinding{}); err != nil {
		return fmt.Errorf("deleting role binding: %w", err)
	}

	account := types.NamespacedName{Namespace: s.TargetNamespace, Name: name}
	if err := s.ensureResourceDeleted(ctx, account, &corev1.ServiceAccount{}); err != nil {
		return fmt.Errorf("deleting service account: %w", err)
	}

	return nil
}

// ensureServiceAccount keeps the account in its desired shape. The account exists
// only as a subject name — helm-controller impersonates it with headers on top of
// its own identity — so no token is ever mounted for it.
func (s *AccessService) ensureServiceAccount(ctx context.Context, rel source.Release, name string) error {
	account := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: s.TargetNamespace},
	}

	_, err := controllerutil.CreateOrPatch(ctx, s.Client, account, func() error {
		account.Labels = rel.SourceLabels()
		account.AutomountServiceAccountToken = ptr.To(false)

		return nil
	})

	return err
}

// seedRole creates the namespace Role with full rights inside the namespace when it
// does not exist, and leaves an existing one untouched: not read, not compared, not
// patched. CreateOrPatch would overwrite an owner's edit; only Create fits.
func (s *AccessService) seedRole(ctx context.Context, namespace string) error {
	role := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ApplicationRoleName,
			Namespace: namespace,
			Labels: map[string]string{
				helmv1alpha1.LabelManagedBy: helmv1alpha1.LabelManagedByValue,
			},
		},
		Rules: []rbacv1.PolicyRule{{
			APIGroups: []string{"*"},
			Resources: []string{"*"},
			Verbs:     []string{"*"},
		}},
	}

	return client.IgnoreAlreadyExists(s.Client.Create(ctx, role))
}

// ensureRoleBinding binds the account to the namespace Role. roleRef is immutable in
// Kubernetes, so it is set only when the binding is created; the subjects and labels
// are reconciled on every pass.
func (s *AccessService) ensureRoleBinding(ctx context.Context, rel source.Release, name string) error {
	binding := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: rel.TargetNamespace()},
	}

	_, err := controllerutil.CreateOrPatch(ctx, s.Client, binding, func() error {
		binding.Labels = rel.SourceLabels()

		if binding.RoleRef.Name == "" {
			binding.RoleRef = rbacv1.RoleRef{
				APIGroup: rbacv1.GroupName,
				Kind:     "Role",
				Name:     ApplicationRoleName,
			}
		}

		binding.Subjects = []rbacv1.Subject{{
			Kind:      rbacv1.ServiceAccountKind,
			Name:      name,
			Namespace: s.TargetNamespace,
		}}

		return nil
	})

	return err
}
