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

package rbac

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"reflect"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
	"github.com/deckhouse/operator-helm/internal/contract"
	"github.com/deckhouse/operator-helm/internal/status"
	"github.com/deckhouse/operator-helm/internal/utils"
)

// ApplicationRoleName is the Role every application release of a namespace is bound to.
// There is one per namespace, and it outlives the applications bound to it.
const ApplicationRoleName = "operator-helm-application"

// Identity provides the identity a release is applied with: a ServiceAccount in the operator
// namespace, a Role in the target namespace and a RoleBinding tying the two together.
type Identity struct {
	// Reads bypass the manager's cache, which holds no accounts and only labelled bindings,
	// so an unlabelled foreign object is seen rather than read as absent.
	client          client.Client
	targetNamespace string
}

func NewIdentity(c client.Client, targetNamespace string) *Identity {
	return &Identity{client: c, targetNamespace: targetNamespace}
}

// Ensure reconciles the account, the Role and the binding. A release without a service
// account name belongs to a family that does not impersonate.
func (s *Identity) Ensure(ctx context.Context, rel contract.Release) IdentityOutcome {
	name := rel.InternalNames().ServiceAccount
	if name == "" {
		return IdentityOutcome{}
	}

	namespace := rel.TargetNamespace()

	if err := s.ensureServiceAccount(ctx, rel, name); err != nil {
		return rbacFailure(fmt.Errorf("ensuring service account: %w", err))
	}

	if err := s.ensureRole(ctx, namespace); err != nil {
		return rbacFailure(fmt.Errorf("ensuring role: %w", err))
	}

	if err := s.ensureRoleBinding(ctx, rel, namespace, name); err != nil {
		return rbacFailure(fmt.Errorf("ensuring role binding: %w", err))
	}

	return IdentityOutcome{}
}

// foreignObjectError marks an object occupying a derived name without the managed-by label.
type foreignObjectError struct {
	message string
}

func (e *foreignObjectError) Error() string { return e.message }

// rbacFailure names the failure the release reports; a foreign object is terminal.
func rbacFailure(err error) IdentityOutcome {
	var foreign *foreignObjectError
	if errors.As(err, &foreign) {
		return IdentityOutcome{
			Err:      err,
			Terminal: true,
			Reason:   helmv1alpha1.ReasonForeignRBACObject,
			Message:  foreign.message,
		}
	}

	return IdentityOutcome{
		Err:     err,
		Reason:  helmv1alpha1.ReasonRBACSetupFailed,
		Message: "Failed to set up the release identity: " + err.Error(),
	}
}

// managedByOperator reports whether an object found under a derived name is one of ours.
func managedByOperator(labels map[string]string) bool {
	return labels[helmv1alpha1.LabelManagedBy] == helmv1alpha1.LabelManagedByValue
}

// createOrPatchOwned is controllerutil.CreateOrPatch with mutate told whether the read
// found an existing object, so ownership can be judged before the write.
func createOrPatchOwned(ctx context.Context, c client.Client, obj client.Object, mutate func(exists bool) error) error {
	if err := c.Get(ctx, client.ObjectKeyFromObject(obj), obj); err != nil {
		if !apierrors.IsNotFound(err) {
			return err
		}

		if err := mutate(false); err != nil {
			return err
		}

		return c.Create(ctx, obj)
	}

	base := obj.DeepCopyObject().(client.Object)

	if err := mutate(true); err != nil {
		return err
	}

	if reflect.DeepEqual(base, obj) {
		return nil
	}

	// The optimistic lock makes the ownership test and the write one decision.
	return c.Patch(ctx, obj, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
}

// Cleanup removes the account and the binding and returns whichever is still present, for
// the caller to wait on. The shared namespace Role stays and is never waited on.
func (s *Identity) Cleanup(ctx context.Context, rel contract.Release) (status.DeletingResource, error) {
	name := rel.InternalNames().ServiceAccount
	if name == "" {
		return nil, nil
	}

	bindingKey := types.NamespacedName{Namespace: rel.TargetNamespace(), Name: name}
	binding, err := s.ensureOwnedRoleBindingDeleted(ctx, bindingKey)
	if err != nil {
		return nil, fmt.Errorf("deleting role binding: %w", err)
	}

	// Deleted by name alone: nobody else can pre-create anything in the operator's namespace.
	account := &corev1.ServiceAccount{}
	accountKey := types.NamespacedName{Namespace: s.targetNamespace, Name: name}

	accountExists, err := utils.DeleteIfPresent(ctx, s.client, accountKey, account)
	if err != nil {
		return nil, fmt.Errorf("deleting service account: %w", err)
	}

	// Both deletes are issued before either is waited on, so one stuck object cannot stall the other.
	switch {
	case binding != nil:
		return pendingDeletion{binding}, nil
	case accountExists:
		return pendingDeletion{account}, nil
	default:
		return nil, nil
	}
}

// RemoveAccount deletes the ServiceAccount and nothing else, and waits for nothing.
// helm-controller v1.6.4 skips the uninstall when the impersonated account is absent.
func (s *Identity) RemoveAccount(ctx context.Context, rel contract.Release) error {
	name := rel.InternalNames().ServiceAccount
	if name == "" {
		return nil
	}

	account := &corev1.ServiceAccount{}
	key := types.NamespacedName{Namespace: s.targetNamespace, Name: name}

	if _, err := utils.DeleteIfPresent(ctx, s.client, key, account); err != nil {
		return fmt.Errorf("deleting service account: %w", err)
	}

	return nil
}

// pendingDeletion adapts an RBAC object to status.DeletingResource. It reports no
// conditions, so the owner always reads it as a deletion still in progress.
type pendingDeletion struct {
	client.Object
}

func (pendingDeletion) GetConditions() []metav1.Condition { return nil }

// ensureServiceAccount keeps the account in its desired shape; no token is mounted for it.
func (s *Identity) ensureServiceAccount(ctx context.Context, rel contract.Release, name string) error {
	account := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: s.targetNamespace},
	}

	_, err := controllerutil.CreateOrPatch(ctx, s.client, account, func() error {
		// Merged rather than replaced, so labels set by someone else survive.
		if account.Labels == nil {
			account.Labels = map[string]string{}
		}
		maps.Copy(account.Labels, rel.SourceLabels())
		account.AutomountServiceAccountToken = ptr.To(false)

		return nil
	})

	return err
}

// ensureRole keeps the namespace Role in its desired shape: full rights inside the namespace,
// and nothing outside it. A Role found under the name that is not ours is refused.
func (s *Identity) ensureRole(ctx context.Context, namespace string) error {
	// The Role is shared, so two applications can both create it; the loser retries as a patch.
	err := s.applyRole(ctx, namespace)
	if apierrors.IsAlreadyExists(err) {
		err = s.applyRole(ctx, namespace)
	}

	return err
}

func (s *Identity) applyRole(ctx context.Context, namespace string) error {
	role := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: ApplicationRoleName, Namespace: namespace},
	}

	return createOrPatchOwned(ctx, s.client, role, func(exists bool) error {
		if exists && !managedByOperator(role.Labels) {
			return &foreignObjectError{message: fmt.Sprintf(
				"role %s/%s already exists and is not managed by the operator",
				namespace, ApplicationRoleName,
			)}
		}

		// Merged rather than replaced, so labels set by someone else survive.
		if role.Labels == nil {
			role.Labels = map[string]string{}
		}
		maps.Copy(role.Labels, applicationRBACLabels())

		role.Rules = []rbacv1.PolicyRule{{
			APIGroups: []string{"*"},
			Resources: []string{"*"},
			Verbs:     []string{"*"},
		}}

		return nil
	})
}

// applicationRBACLabels mark the namespace Role and every role binding we own.
func applicationRBACLabels() map[string]string {
	return map[string]string{
		helmv1alpha1.LabelManagedBy: helmv1alpha1.LabelManagedByValue,
	}
}

func applicationRoleRef() rbacv1.RoleRef {
	return rbacv1.RoleRef{
		APIGroup: rbacv1.GroupName,
		Kind:     "Role",
		Name:     ApplicationRoleName,
	}
}

// ensureRoleBinding binds the account to the namespace Role, reconciling a binding of ours
// on every pass and leaving one that is not ours exactly as it is.
func (s *Identity) ensureRoleBinding(ctx context.Context, rel contract.Release, namespace, name string) error {
	desiredRef := applicationRoleRef()

	existing := &rbacv1.RoleBinding{}
	switch err := s.client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, existing); {
	case apierrors.IsNotFound(err):
	case err != nil:
		return err
	case !managedByOperator(existing.Labels):
		return &foreignObjectError{message: fmt.Sprintf(
			"role binding %s/%s already exists and is not managed by the operator", namespace, name,
		)}
	case existing.RoleRef != desiredRef:
		// roleRef is immutable, so a binding naming another role is replaced rather than patched.
		err := s.client.Delete(ctx, existing, client.Preconditions{
			UID: &existing.UID, ResourceVersion: &existing.ResourceVersion,
		})
		if client.IgnoreNotFound(err) != nil {
			return err
		}
	}

	binding := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
	}

	return createOrPatchOwned(ctx, s.client, binding, func(exists bool) error {
		// The ownership test is repeated on this read: another binding may have taken the name.
		if exists && !managedByOperator(binding.Labels) {
			return &foreignObjectError{message: fmt.Sprintf(
				"role binding %s/%s already exists and is not managed by the operator", namespace, name,
			)}
		}

		if binding.Labels == nil {
			binding.Labels = map[string]string{}
		}
		maps.Copy(binding.Labels, rel.SourceLabels())
		maps.Copy(binding.Labels, applicationRBACLabels())
		binding.RoleRef = desiredRef

		binding.Subjects = []rbacv1.Subject{{
			Kind:      rbacv1.ServiceAccountKind,
			Name:      name,
			Namespace: s.targetNamespace,
		}}

		return nil
	})
}

// ensureOwnedRoleBindingDeleted deletes the role binding at nn only when it carries our
// managed-by label. It returns the binding while still present, and nil otherwise.
func (s *Identity) ensureOwnedRoleBindingDeleted(ctx context.Context, nn types.NamespacedName) (*rbacv1.RoleBinding, error) {
	binding := &rbacv1.RoleBinding{}
	if err := s.client.Get(ctx, nn, binding); err != nil {
		return nil, client.IgnoreNotFound(err)
	}

	if !managedByOperator(binding.Labels) {
		return nil, nil
	}

	// Only this exact object at this exact revision, so a replacement is not deleted for it.
	switch err := s.client.Delete(ctx, binding, client.Preconditions{
		UID: &binding.UID, ResourceVersion: &binding.ResourceVersion,
	}); {
	case apierrors.IsNotFound(err):
		return nil, nil
	case err != nil:
		return nil, err
	default:
		return binding, nil
	}
}
