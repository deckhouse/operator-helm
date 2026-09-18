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
	"maps"

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
//
// Neither the account nor the binding triggers a reconcile on its own: nothing
// watches either kind, so an out-of-band deletion of either is not noticed
// immediately and is repaired on the release's next reconcile. Both are also
// excluded from the manager's client cache (see cmd/operator-helm-controller),
// so reading them here never starts a cluster-wide informer for the kind.
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

	namespace := rel.TargetNamespace()

	if err := s.ensureServiceAccount(ctx, rel, name); err != nil {
		return fmt.Errorf("ensuring service account: %w", err)
	}

	if err := s.seedRole(ctx, namespace); err != nil {
		return fmt.Errorf("seeding role: %w", err)
	}

	if err := s.ensureRoleBinding(ctx, rel, namespace, name); err != nil {
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
	if err := s.ensureOwnedRoleBindingDeleted(ctx, binding); err != nil {
		return fmt.Errorf("deleting role binding: %w", err)
	}

	// Deleted by name alone, unlike the binding above: the account lives in
	// s.TargetNamespace (the operator's own namespace), where a namespace owner
	// has no access to pre-create anything under our name. There is no foreign
	// object to protect here, so the ownership check the binding needs does not
	// apply.
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
		// Merge rather than replace: the account may carry labels put there by
		// someone else (a policy engine, a cost allocator), and dropping them on
		// every pass would fight whoever set them.
		if account.Labels == nil {
			account.Labels = map[string]string{}
		}
		maps.Copy(account.Labels, rel.SourceLabels())
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

// applicationRoleRef is the roleRef every role binding we own carries.
func applicationRoleRef() rbacv1.RoleRef {
	return rbacv1.RoleRef{
		APIGroup: rbacv1.GroupName,
		Kind:     "Role",
		Name:     ApplicationRoleName,
	}
}

// ensureRoleBinding binds the account to the namespace Role. roleRef is immutable
// in Kubernetes, so it is only ever written on create or written back unchanged;
// the subjects and labels are reconciled on every pass.
//
// A binding that already exists under our name but points at a different role is
// not ours to reuse: adding our account as its subject would grant that account
// whatever the foreign role grants. Such a binding is reported and left exactly as
// it is — never patched, never deleted — so its owner decides what happens to it.
func (s *AccessService) ensureRoleBinding(ctx context.Context, rel source.Release, namespace, name string) error {
	desiredRef := applicationRoleRef()

	binding := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
	}

	_, err := controllerutil.CreateOrPatch(ctx, s.Client, binding, func() error {
		// roleRef.Name is required by API validation, so it is empty only here: a
		// fresh object about to be created. Anything already stored carries it, so a
		// mismatch here can only mean an existing binding that points elsewhere.
		if binding.RoleRef.Name != "" && binding.RoleRef != desiredRef {
			return fmt.Errorf(
				"role binding %s/%s already binds %s/%s; refusing to adopt it",
				namespace, name, binding.RoleRef.Kind, binding.RoleRef.Name,
			)
		}

		// Merge rather than replace: the binding lives in the user's namespace, where
		// a cluster's own policy or cost labelling is most likely to land, and
		// dropping such labels on every pass would fight whoever set them.
		if binding.Labels == nil {
			binding.Labels = map[string]string{}
		}
		maps.Copy(binding.Labels, rel.SourceLabels())
		binding.RoleRef = desiredRef

		binding.Subjects = []rbacv1.Subject{{
			Kind:      rbacv1.ServiceAccountKind,
			Name:      name,
			Namespace: s.TargetNamespace,
		}}

		return nil
	})

	return err
}

// ensureOwnedRoleBindingDeleted deletes the role binding at nn only when it is
// ours to delete: it carries our roleRef and our managed-by label. The derived
// name is fully computable by anyone, so a binding found under it may belong to
// someone else — the same reason ensureRoleBinding refuses to adopt a foreign
// binding on create. A binding that is not ours is left alone; that is not an
// error and must not block the rest of the cleanup.
func (s *AccessService) ensureOwnedRoleBindingDeleted(ctx context.Context, nn types.NamespacedName) error {
	binding := &rbacv1.RoleBinding{}
	if err := s.Client.Get(ctx, nn, binding); err != nil {
		return client.IgnoreNotFound(err)
	}

	if binding.RoleRef != applicationRoleRef() {
		return nil
	}
	if binding.Labels[helmv1alpha1.LabelManagedBy] != helmv1alpha1.LabelManagedByValue {
		return nil
	}

	return client.IgnoreNotFound(s.Client.Delete(ctx, binding))
}
