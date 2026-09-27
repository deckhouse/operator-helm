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

// ApplicationRoleName is the Role every application release of a namespace is bound
// to. One Role per namespace: every application there is bound to the same object,
// and it outlives the applications that are bound to it.
const ApplicationRoleName = "operator-helm-application"

// Identity provides the identity a release is applied with: a ServiceAccount in
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
type Identity struct {
	client          client.Client
	targetNamespace string
}

func NewIdentity(c client.Client, targetNamespace string) *Identity {
	return &Identity{client: c, targetNamespace: targetNamespace}
}

// Ensure reconciles the account, the Role and the binding. A release without a
// service account name belongs to a family that does not impersonate; nothing is
// created for it.
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

// foreignObjectError marks an object occupying a name the operator derives that is
// not the operator's to touch. The derived names are fully computable by anyone, so
// finding something under one of them says nothing about who put it there; the
// managed-by label does.
type foreignObjectError struct {
	message string
}

func (e *foreignObjectError) Error() string { return e.message }

// rbacFailure names the failure the release reports. A foreign object is terminal:
// it leaves the way only by being removed or labelled, and neither is something a
// retry brings about — the informers behind the watches on both kinds select on the
// managed-by label, so an object without it is not even observed going away.
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

// managedByOperator reports whether an object found under a derived name is one of
// ours. It is the single ownership test: everything that carries the label is ours
// to shape, everything that does not is left untouched.
func managedByOperator(labels map[string]string) bool {
	return labels[helmv1alpha1.LabelManagedBy] == helmv1alpha1.LabelManagedByValue
}

// createOrPatchOwned is controllerutil.CreateOrPatch for an object whose ownership
// has to be judged before it is written: mutate is called with what the read found
// (exists tells an object that is already there from one about to be created), and
// the patch that follows carries the resource version it was read at.
//
// That resource version is what makes the ownership test and the write one
// decision. Without it the test says the object carried our label a moment ago,
// not that the object being written is the one that carried it: an object deleted
// and recreated by someone else in between would take our patch, and the label is
// the privilege boundary of this package. Under the lock such a swap is a conflict
// instead — a transient failure the release is retried from, and the next pass
// judges whatever it then finds on its own labels.
//
// An unchanged object is not written at all, as CreateOrPatch does not write one:
// a patch every pass would bump the resource version and wake the watches on the
// Role and the binding for nothing.
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

	return c.Patch(ctx, obj, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
}

// Cleanup removes the account and the binding and returns whichever of them is
// still present, so the caller waits for it to actually go away before the
// release's own finalizer comes off — the same shape the internal flux objects are
// torn down with (see internalobj.Releases.Cleanup and
// pass.AwaitInternalResourceDeletion). A Delete only sets a deletion timestamp:
// a foreign finalizer on the account or the binding keeps it Terminating, and
// neither object carries an ownerReference, so an application that dropped its
// finalizer at that point would leave it behind for good — and the next
// application of the namespace would then find it under the derived name and fail
// terminally as foreign. It returns nil once both are gone.
//
// The Role stays: it is shared by every application of the namespace, and on its
// own — with no binding left naming it — it grants nothing. Nothing waits on it
// either, for the same reason: it is not this application's to outlive.
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

	// Deleted by name alone, unlike the binding above: the account lives in
	// s.targetNamespace (the operator's own namespace), where a namespace owner
	// has no access to pre-create anything under our name. There is no foreign
	// object to protect here, so the ownership check the binding needs does not
	// apply.
	account := &corev1.ServiceAccount{}
	accountKey := types.NamespacedName{Namespace: s.targetNamespace, Name: name}

	accountExists, err := utils.DeleteIfPresent(ctx, s.client, accountKey, account)
	if err != nil {
		return nil, fmt.Errorf("deleting service account: %w", err)
	}

	// Both deletes are issued before either is waited on: they are independent, and
	// holding the account's delete back until a binding stuck on someone else's
	// finalizer goes away would stall a deletion that could have made progress.
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
//
// It is the escape from a teardown that cannot be applied as this identity at all:
// the namespace the release lives in is terminating, so the namespace controller
// deletes the Role and the RoleBinding there while the account — which lives in the
// operator's namespace — survives, leaving helm-controller impersonating an account
// that exists and may do nothing. Its uninstall reads the Helm storage secret in
// that namespace and is refused, on every attempt, forever. helm-controller skips
// the uninstall when the account is absent (v1.6.4, the "Confirm any ServiceAccount
// used for impersonation exists" check in internal/controller/helmrelease_controller.go,
// which tests NotFound on a Get of it), so taking the account away is what lets the
// internal HelmRelease finish.
//
// Nothing is returned to wait on: the caller reaches Cleanup later in the same
// teardown, and that is where the account — gone or still Terminating on someone
// else's finalizer — is waited for as it always was.
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

// pendingDeletion adapts an RBAC object to status.DeletingResource. Unlike the
// internal flux objects that interface was written for, a ServiceAccount or a
// RoleBinding has no conditions to report: whatever holds it in Terminating is a
// finalizer of someone else's, which says nothing about itself on the object. The
// owner therefore reports it as a deletion still in progress and never as a failed
// one, which is exactly what status.MarkDeletionPending does without a Ready
// condition to read.
type pendingDeletion struct {
	client.Object
}

func (pendingDeletion) GetConditions() []metav1.Condition { return nil }

// ensureServiceAccount keeps the account in its desired shape. The account exists
// only as a subject name — helm-controller impersonates it with headers on top of
// its own identity — so no token is ever mounted for it.
func (s *Identity) ensureServiceAccount(ctx context.Context, rel contract.Release, name string) error {
	account := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: s.targetNamespace},
	}

	_, err := controllerutil.CreateOrPatch(ctx, s.client, account, func() error {
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
func (s *Identity) ensureRole(ctx context.Context, namespace string) error {
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
}

// applicationRBACLabels mark the namespace Role and every role binding we own. The
// managed-by label is both the ownership test and what the informers behind the
// watches on both kinds select on, so losing it reads as a deletion, brings the
// object back here and has it refused as foreign.
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
		// The preconditions keep the delete off a binding that was replaced or
		// rewritten between the read and here: that one has to be judged on its own
		// labels, and the conflict brings this pass back to do it. A binding that is
		// simply gone by now needs no deleting — the create below is what it was
		// heading for.
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
		// The object is read again here, so the ownership test is repeated on what
		// that read found: between the read above and this one a binding of someone
		// else's may have taken the name, and merging our labels onto it is the
		// adoption the test exists to prevent.
		if exists && !managedByOperator(binding.Labels) {
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
			Namespace: s.targetNamespace,
		}}

		return nil
	})
}

// ensureOwnedRoleBindingDeleted deletes the role binding at nn only when it carries
// our managed-by label. The derived name is fully computable by anyone, so a binding
// found under it may belong to someone else — the same ownership test ensureRoleBinding
// applies before it touches anything. A binding that is not ours is left alone; that
// is not an error and must not block the rest of the cleanup.
//
// It returns the binding while it is still present — a delete that was issued is
// not a delete that finished — and nil for a binding that is gone or was never
// ours to remove: neither is something this release waits on.
func (s *Identity) ensureOwnedRoleBindingDeleted(ctx context.Context, nn types.NamespacedName) (*rbacv1.RoleBinding, error) {
	binding := &rbacv1.RoleBinding{}
	if err := s.client.Get(ctx, nn, binding); err != nil {
		return nil, client.IgnoreNotFound(err)
	}

	if !managedByOperator(binding.Labels) {
		return nil, nil
	}

	// Only this exact object at this exact revision: a binding deleted and recreated
	// by someone else between the read and here would otherwise be deleted on the
	// strength of a label it never carried. The conflict is transient — the pass that
	// retries reads the replacement and leaves it alone as foreign.
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
