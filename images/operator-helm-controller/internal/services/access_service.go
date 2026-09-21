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
	"errors"
	"fmt"
	"maps"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
	"github.com/deckhouse/operator-helm/internal/source"
)

// ApplicationRoleName is the Role every application release of a namespace is bound
// to. One Role per namespace: every application there is bound to the same object,
// and it outlives the applications that are bound to it.
const ApplicationRoleName = "operator-helm-application"

// AccessService provides the identity a release is applied with: a ServiceAccount in
// the operator namespace (next to the HelmRelease, where helm-controller looks it
// up), a Role in the target namespace and a RoleBinding tying the two together.
//
// The Role is bound through a RoleBinding, so whatever it grants stops at the
// namespace boundary: cluster-scoped resources are unreachable regardless of its
// content.
//
// All three are reconciled on every pass: the rules of the Role and the subjects of
// the binding are ours and are written back, so an edit made out of band does not
// survive. The Role and the binding are also watched (see
// internal/controller/helmapplication), which is what turns such an edit into a
// reconcile of the applications concerned rather than a drift that lasts until the
// next pass happens for another reason. All three kinds are excluded from the
// manager's client cache (see cmd/operator-helm-controller), so the reads here go
// to the API server and never depend on the label-scoped informers those watches
// run on.
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

// EnsureAccess reconciles the account, the Role and the binding. A release without a
// service account name belongs to a family that does not impersonate; nothing is
// created for it.
func (s *AccessService) EnsureAccess(ctx context.Context, rel source.Release) AccessOutcome {
	name := rel.InternalNames().ServiceAccount
	if name == "" {
		return AccessOutcome{}
	}

	namespace := rel.TargetNamespace()

	if err := s.ensureServiceAccount(ctx, rel, name); err != nil {
		return accessFailure(fmt.Errorf("ensuring service account: %w", err))
	}

	if err := s.ensureRole(ctx, namespace); err != nil {
		return accessFailure(fmt.Errorf("ensuring role: %w", err))
	}

	if err := s.ensureRoleBinding(ctx, rel, namespace, name); err != nil {
		return accessFailure(fmt.Errorf("ensuring role binding: %w", err))
	}

	return AccessOutcome{}
}

// foreignObjectError marks an object occupying a name the operator derives that is
// not the operator's to touch. The derived names are fully computable by anyone, so
// finding something under one of them says nothing about who put it there; the
// managed-by label does.
type foreignObjectError struct {
	message string
}

func (e *foreignObjectError) Error() string { return e.message }

// accessFailure names the failure the release reports. A foreign object is terminal:
// it leaves the way only by being removed or labelled, and neither is something a
// retry brings about — the informers behind the watches on both kinds select on the
// managed-by label, so an object without it is not even observed going away.
func accessFailure(err error) AccessOutcome {
	var foreign *foreignObjectError
	if errors.As(err, &foreign) {
		return AccessOutcome{
			Err:      err,
			Terminal: true,
			Reason:   helmv1alpha1.ReasonForeignAccessObject,
			Message:  foreign.message,
		}
	}

	return AccessOutcome{
		Err:     err,
		Reason:  helmv1alpha1.ReasonAccessSetupFailed,
		Message: "Failed to set up the release identity: " + err.Error(),
	}
}

// managedByOperator reports whether an object found under a derived name is one of
// ours. It is the single ownership test: everything that carries the label is ours
// to shape, everything that does not is left untouched.
func managedByOperator(labels map[string]string) bool {
	return labels[helmv1alpha1.LabelManagedBy] == helmv1alpha1.LabelManagedByValue
}

// CleanupAccess removes the account and the binding. The Role stays: it is shared by
// every application of the namespace, and on its own — with no binding left naming
// it — it grants nothing.
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

// ensureRole keeps the namespace Role in its desired shape: full rights inside the
// namespace, and nothing outside it. Unlike the account and the binding the Role is
// shared by every application of the namespace, so it is not derived from one
// release — its rules are ours whatever put it there, and a narrowed Role is written
// back to them on the next pass. Its name is fixed and computable by anyone, though,
// so a Role found under it that is not ours is refused rather than seized: granting
// every application of the namespace full rights through an object someone else owns
// is not a decision this controller gets to make on their behalf.
//
// Being shared is also what makes the create race real: two applications of one
// namespace reconciling at once both read the Role as missing and both create it,
// and the loser is refused. They write the same content, so the second attempt is
// the patch the loser would have made had it read the Role a moment later.
func (s *AccessService) ensureRole(ctx context.Context, namespace string) error {
	err := s.applyRole(ctx, namespace)
	if apierrors.IsAlreadyExists(err) {
		err = s.applyRole(ctx, namespace)
	}

	return err
}

func (s *AccessService) applyRole(ctx context.Context, namespace string) error {
	role := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: ApplicationRoleName, Namespace: namespace},
	}

	_, err := controllerutil.CreateOrPatch(ctx, s.Client, role, func() error {
		// A resource version is set only on an object that was read back, so it is
		// what tells a Role that is already there from one about to be created.
		if role.ResourceVersion != "" && !managedByOperator(role.Labels) {
			return &foreignObjectError{message: fmt.Sprintf(
				"role %s/%s already exists and is not managed by the operator",
				namespace, ApplicationRoleName,
			)}
		}

		// Merge rather than replace, for the same reason the account and the binding
		// merge theirs: the Role lives in the user's namespace, where a policy engine
		// or a cost allocator is most likely to add a label of its own.
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

	return err
}

// applicationRBACLabels mark the namespace Role and every role binding we own. The
// managed-by label is both the ownership test and what the informers behind the
// watches on both kinds select on, so losing it reads as a deletion, brings the
// object back here and has it refused as foreign; heritage is what the rest of
// Deckhouse recognizes a module's object by.
func applicationRBACLabels() map[string]string {
	return map[string]string{
		helmv1alpha1.LabelManagedBy:         helmv1alpha1.LabelManagedByValue,
		helmv1alpha1.LabelDeckhouseHeritage: helmv1alpha1.LabelDeckhouseHeritageValue,
	}
}

// applicationRoleRef is the roleRef every role binding we own carries.
func applicationRoleRef() rbacv1.RoleRef {
	return rbacv1.RoleRef{
		APIGroup: rbacv1.GroupName,
		Kind:     "Role",
		Name:     ApplicationRoleName,
	}
}

// ensureRoleBinding binds the account to the namespace Role. A binding that is ours
// is held to the shape we want whatever state it is found in: its subjects and
// labels are reconciled on every pass, and a roleRef naming some other role is
// corrected too — by replacing the object, because roleRef is immutable in
// Kubernetes and cannot be patched.
//
// A binding that is not ours is a different matter. The name is derived and fully
// computable, so it may well have been taken by someone else; adding our account to
// such a binding would grant it whatever that binding grants. It is reported and
// left exactly as it is — never patched, never deleted — so its owner decides what
// happens to it.
func (s *AccessService) ensureRoleBinding(ctx context.Context, rel source.Release, namespace, name string) error {
	desiredRef := applicationRoleRef()

	existing := &rbacv1.RoleBinding{}
	switch err := s.Client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, existing); {
	case apierrors.IsNotFound(err):
	case err != nil:
		return err
	case !managedByOperator(existing.Labels):
		return &foreignObjectError{message: fmt.Sprintf(
			"role binding %s/%s already exists and is not managed by the operator", namespace, name,
		)}
	case existing.RoleRef != desiredRef:
		// The precondition keeps the delete off a binding that was replaced between
		// the read and here: that one has to be judged on its own labels, and the
		// conflict brings this pass back to do it. A binding that is simply gone by
		// now needs no deleting — the create below is what it was heading for.
		err := s.Client.Delete(ctx, existing, client.Preconditions{UID: &existing.UID})
		if client.IgnoreNotFound(err) != nil {
			return err
		}
	}

	binding := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
	}

	_, err := controllerutil.CreateOrPatch(ctx, s.Client, binding, func() error {
		// CreateOrPatch reads the object again, so the ownership test is repeated on
		// what it found: between the read above and this one a binding of someone
		// else's may have taken the name, and merging our labels onto it is the
		// adoption the test exists to prevent.
		if binding.ResourceVersion != "" && !managedByOperator(binding.Labels) {
			return &foreignObjectError{message: fmt.Sprintf(
				"role binding %s/%s already exists and is not managed by the operator", namespace, name,
			)}
		}

		// Merge rather than replace: the binding lives in the user's namespace, where
		// a cluster's own policy or cost labelling is most likely to land, and
		// dropping such labels on every pass would fight whoever set them.
		if binding.Labels == nil {
			binding.Labels = map[string]string{}
		}
		maps.Copy(binding.Labels, rel.SourceLabels())
		maps.Copy(binding.Labels, applicationRBACLabels())
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

// ensureOwnedRoleBindingDeleted deletes the role binding at nn only when it carries
// our managed-by label. The derived name is fully computable by anyone, so a binding
// found under it may belong to someone else — the same ownership test ensureRoleBinding
// applies before it touches anything. A binding that is not ours is left alone; that
// is not an error and must not block the rest of the cleanup.
func (s *AccessService) ensureOwnedRoleBindingDeleted(ctx context.Context, nn types.NamespacedName) error {
	binding := &rbacv1.RoleBinding{}
	if err := s.Client.Get(ctx, nn, binding); err != nil {
		return client.IgnoreNotFound(err)
	}

	if !managedByOperator(binding.Labels) {
		return nil
	}

	return client.IgnoreNotFound(s.Client.Delete(ctx, binding))
}
