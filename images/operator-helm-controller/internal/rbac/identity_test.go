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
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
	"github.com/deckhouse/operator-helm/internal/adapter"
)

func testApplication() *helmv1alpha1.HelmApplication {
	return &helmv1alpha1.HelmApplication{
		ObjectMeta: metav1.ObjectMeta{Name: "my-app", Namespace: "team-a", Generation: 1},
		Spec: helmv1alpha1.HelmApplicationSpec{
			Chart: helmv1alpha1.HelmApplicationChartRef{Name: "podinfo", Repository: "stable", Version: "6.7.1"},
		},
	}
}

func newIdentity(t *testing.T, objects ...client.Object) (*Identity, client.Client) {
	t.Helper()

	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objects...).Build()

	return NewIdentity(c, testNamespace, true), c
}

func TestIdentityEnsureCreatesTheIdentity(t *testing.T) {
	rel := adapter.NewApplicationRelease(testApplication())
	service, c := newIdentity(t)

	if out := service.Ensure(context.Background(), rel); out.Err != nil {
		t.Fatalf("Ensure returned %v", out.Err)
	}

	names := rel.InternalNames()

	sa := &corev1.ServiceAccount{}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: names.ServiceAccount}, sa); err != nil {
		t.Fatalf("service account was not created in the operator namespace: %v", err)
	}
	if sa.AutomountServiceAccountToken == nil || *sa.AutomountServiceAccountToken {
		t.Fatal("the account is only a subject name: no token must be mounted for it")
	}
	if !reflect.DeepEqual(sa.Labels, rel.SourceLabels()) {
		t.Fatalf("service account labels = %v, want %v", sa.Labels, rel.SourceLabels())
	}

	var roles rbacv1.RoleList
	if err := c.List(context.Background(), &roles, client.InNamespace("team-a")); err != nil {
		t.Fatalf("listing roles: %v", err)
	}
	if len(roles.Items) != 0 {
		t.Fatalf("roles = %+v, want none: the rights come from the namespace admin ClusterRole", roles.Items)
	}

	binding := &rbacv1.RoleBinding{}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "team-a", Name: names.ServiceAccount}, binding); err != nil {
		t.Fatalf("role binding was not created in the application namespace: %v", err)
	}
	if binding.RoleRef != (rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "d8:namespace:admin"}) {
		t.Fatalf("roleRef = %+v", binding.RoleRef)
	}
	wantSubjects := []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: names.ServiceAccount, Namespace: testNamespace}}
	if !reflect.DeepEqual(binding.Subjects, wantSubjects) {
		t.Fatalf("subjects = %+v, want %+v", binding.Subjects, wantSubjects)
	}
	for key, want := range applicationRBACLabels() {
		if binding.Labels[key] != want {
			t.Fatalf("role binding label %q = %q, want %q", key, binding.Labels[key], want)
		}
	}
	for key, want := range rel.SourceLabels() {
		if binding.Labels[key] != want {
			t.Fatalf("role binding label %q = %q, want %q", key, binding.Labels[key], want)
		}
	}
}

func TestIdentityEnsureBindsTheNamespaceAdminRoleOfTheRoleModel(t *testing.T) {
	tests := []struct {
		name   string
		rbacV2 bool
		want   string
	}{
		{name: "current role model", rbacV2: true, want: "d8:namespace:admin"},
		{name: "previous role model", rbacV2: false, want: "d8:use:role:admin"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rel := adapter.NewApplicationRelease(testApplication())
			c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()

			if out := NewIdentity(c, testNamespace, tt.rbacV2).Ensure(context.Background(), rel); out.Err != nil {
				t.Fatalf("Ensure returned %v", out.Err)
			}

			binding := &rbacv1.RoleBinding{}
			key := client.ObjectKey{Namespace: "team-a", Name: rel.InternalNames().ServiceAccount}
			if err := c.Get(context.Background(), key, binding); err != nil {
				t.Fatalf("getting role binding: %v", err)
			}
			if want := (rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: tt.want}); binding.RoleRef != want {
				t.Fatalf("roleRef = %+v, want %+v", binding.RoleRef, want)
			}
		})
	}
}

func TestIdentityEnsureRefusesABindingThatIsNotOurs(t *testing.T) {
	rel := adapter.NewApplicationRelease(testApplication())
	foreign := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: rel.InternalNames().ServiceAccount, Namespace: "team-a"},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: "someone-elses-role"},
		Subjects:   []rbacv1.Subject{{Kind: rbacv1.UserKind, Name: "someone-else"}},
	}
	service, c := newIdentity(t, foreign)

	out := service.Ensure(context.Background(), rel)
	if out.Err == nil {
		t.Fatal("a binding that is not ours must be reported, not adopted")
	}
	if !out.Terminal {
		t.Fatalf("outcome = %+v, want it terminal", out)
	}
	if out.Reason != helmv1alpha1.ReasonForeignRBACObject {
		t.Fatalf("reason = %q, want %q", out.Reason, helmv1alpha1.ReasonForeignRBACObject)
	}
	if !strings.Contains(out.Message, "team-a/"+rel.InternalNames().ServiceAccount) {
		t.Fatalf("message %q must name the binding", out.Message)
	}

	stored := &rbacv1.RoleBinding{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(foreign), stored); err != nil {
		t.Fatalf("the foreign binding must survive: %v", err)
	}
	if stored.RoleRef != foreign.RoleRef {
		t.Fatalf("roleRef = %+v, want it untouched", stored.RoleRef)
	}
	if !reflect.DeepEqual(stored.Subjects, foreign.Subjects) {
		t.Fatalf("subjects = %+v, want them untouched", stored.Subjects)
	}
	if len(stored.Labels) != 0 {
		t.Fatalf("labels = %v, want the foreign binding left unlabelled", stored.Labels)
	}
}

func TestIdentityEnsureReplacesOurBindingThatNamesAnotherRole(t *testing.T) {
	rel := adapter.NewApplicationRelease(testApplication())
	names := rel.InternalNames()
	misbound := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      names.ServiceAccount,
			Namespace: "team-a",
			Labels:    applicationRBACLabels(),
		},
		RoleRef:  rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: "some-other-role"},
		Subjects: []rbacv1.Subject{{Kind: rbacv1.UserKind, Name: "someone-else"}},
	}
	service, c := newIdentity(t, misbound)

	if out := service.Ensure(context.Background(), rel); out.Err != nil {
		t.Fatalf("Ensure returned %v", out.Err)
	}

	stored := &rbacv1.RoleBinding{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(misbound), stored); err != nil {
		t.Fatalf("getting role binding: %v", err)
	}
	if stored.RoleRef != applicationRoleRef(true) {
		t.Fatalf("roleRef = %+v, want %+v", stored.RoleRef, applicationRoleRef(true))
	}
	wantSubjects := []rbacv1.Subject{{
		Kind: rbacv1.ServiceAccountKind, Name: names.ServiceAccount, Namespace: testNamespace,
	}}
	if !reflect.DeepEqual(stored.Subjects, wantSubjects) {
		t.Fatalf("subjects = %+v, want %+v", stored.Subjects, wantSubjects)
	}
	for key, want := range applicationRBACLabels() {
		if stored.Labels[key] != want {
			t.Fatalf("binding label %q = %q, want %q", key, stored.Labels[key], want)
		}
	}
}

func swapOnce(t *testing.T, replacement client.Object) (func(ctx context.Context, cl client.WithWatch), *bool) {
	t.Helper()

	swapped := false

	return func(ctx context.Context, cl client.WithWatch) {
		if swapped {
			return
		}
		swapped = true

		current := replacement.DeepCopyObject().(client.Object)
		if err := cl.Get(ctx, client.ObjectKeyFromObject(replacement), current); err != nil {
			t.Fatalf("reading the object about to be swapped: %v", err)
		}
		if err := cl.Delete(ctx, current); err != nil {
			t.Fatalf("swapping the object out: %v", err)
		}
		if err := cl.Create(ctx, replacement.DeepCopyObject().(client.Object)); err != nil {
			t.Fatalf("swapping the object in: %v", err)
		}
	}, &swapped
}

func TestIdentityEnsureRefusesToPatchARoleBindingSwappedAfterTheOwnershipTest(t *testing.T) {
	rel := adapter.NewApplicationRelease(testApplication())
	name := rel.InternalNames().ServiceAccount
	ours := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "team-a", UID: "ours", Labels: applicationRBACLabels()},
		RoleRef:    applicationRoleRef(true),
		Subjects:   []rbacv1.Subject{{Kind: rbacv1.UserKind, Name: "drifted"}},
	}
	theirSubjects := []rbacv1.Subject{{Kind: rbacv1.UserKind, Name: "someone-else"}}
	theirs := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "team-a", UID: "theirs"},
		RoleRef:    applicationRoleRef(true),
		Subjects:   theirSubjects,
	}

	swap, swapped := swapOnce(t, theirs)
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(ours).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				if _, ok := obj.(*rbacv1.RoleBinding); ok {
					swap(ctx, cl)
				}

				return cl.Patch(ctx, obj, patch, opts...)
			},
		}).
		Build()

	out := NewIdentity(c, testNamespace, true).Ensure(context.Background(), rel)
	if !*swapped {
		t.Fatal("the test did not exercise a patch of the role binding")
	}
	if !apierrors.IsConflict(out.Err) {
		t.Fatalf("Ensure returned %v, want the patch rejected as a conflict", out.Err)
	}
	if out.Terminal {
		t.Fatalf("outcome = %+v, want a conflict retried rather than stalled", out)
	}

	stored := &rbacv1.RoleBinding{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(theirs), stored); err != nil {
		t.Fatalf("getting the binding that took the name: %v", err)
	}
	if !reflect.DeepEqual(stored.Subjects, theirSubjects) {
		t.Fatalf("subjects = %+v, want the swapped-in binding untouched", stored.Subjects)
	}
	if len(stored.Labels) != 0 {
		t.Fatalf("labels = %v, want the swapped-in binding left unlabelled", stored.Labels)
	}
}

func TestIdentityCleanupRefusesToDeleteARoleBindingSwappedAfterTheOwnershipTest(t *testing.T) {
	rel := adapter.NewApplicationRelease(testApplication())
	name := rel.InternalNames().ServiceAccount
	ours := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "team-a", UID: "ours", Labels: applicationRBACLabels()},
		RoleRef:    applicationRoleRef(true),
	}
	theirRef := rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: "someone-elses-role"}
	theirs := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "team-a", UID: "theirs"},
		RoleRef:    theirRef,
	}

	swap, swapped := swapOnce(t, theirs)
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(ours).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				if _, ok := obj.(*rbacv1.RoleBinding); ok {
					swap(ctx, cl)
				}

				return cl.Delete(ctx, obj, opts...)
			},
		}).
		Build()

	_, err := NewIdentity(c, testNamespace, true).Cleanup(context.Background(), rel)
	if !*swapped {
		t.Fatal("the test did not exercise a delete of the role binding")
	}
	if !apierrors.IsConflict(err) {
		t.Fatalf("Cleanup returned %v, want the delete rejected as a conflict", err)
	}

	stored := &rbacv1.RoleBinding{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(theirs), stored); err != nil {
		t.Fatalf("the binding that took the name must survive: %v", err)
	}
	if stored.RoleRef != theirRef {
		t.Fatalf("roleRef = %+v, want the swapped-in binding untouched", stored.RoleRef)
	}
}

func TestIdentityEnsureKeepsForeignLabelsOnTheServiceAccountAndBinding(t *testing.T) {
	rel := adapter.NewApplicationRelease(testApplication())
	names := rel.InternalNames()
	service, c := newIdentity(t)

	if out := service.Ensure(context.Background(), rel); out.Err != nil {
		t.Fatalf("first Ensure returned %v", out.Err)
	}

	sa := &corev1.ServiceAccount{}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: names.ServiceAccount}, sa); err != nil {
		t.Fatalf("getting service account: %v", err)
	}
	sa.Labels["cost-center"] = "platform"
	if err := c.Update(context.Background(), sa); err != nil {
		t.Fatalf("labelling service account: %v", err)
	}

	binding := &rbacv1.RoleBinding{}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "team-a", Name: names.ServiceAccount}, binding); err != nil {
		t.Fatalf("getting role binding: %v", err)
	}
	binding.Labels["cost-center"] = "platform"
	if err := c.Update(context.Background(), binding); err != nil {
		t.Fatalf("labelling role binding: %v", err)
	}

	if out := service.Ensure(context.Background(), rel); out.Err != nil {
		t.Fatalf("second Ensure returned %v", out.Err)
	}

	if err := c.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: names.ServiceAccount}, sa); err != nil {
		t.Fatalf("getting service account: %v", err)
	}
	if sa.Labels["cost-center"] != "platform" {
		t.Fatalf("service account labels = %v, want the foreign label kept", sa.Labels)
	}

	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "team-a", Name: names.ServiceAccount}, binding); err != nil {
		t.Fatalf("getting role binding: %v", err)
	}
	if binding.Labels["cost-center"] != "platform" {
		t.Fatalf("role binding labels = %v, want the foreign label kept", binding.Labels)
	}
}

func TestIdentityEnsureIsANoopForAFamilyWithoutAServiceAccount(t *testing.T) {
	service, c := newIdentity(t)

	if out := service.Ensure(context.Background(), adapter.NewAddonRelease(testAddon())); out.Err != nil {
		t.Fatalf("Ensure returned %v", out.Err)
	}

	var accounts corev1.ServiceAccountList
	if err := c.List(context.Background(), &accounts); err != nil {
		t.Fatalf("listing service accounts: %v", err)
	}
	if len(accounts.Items) != 0 {
		t.Fatalf("no identity must be created for a release without a service account name, got %v", accounts.Items)
	}
}

func TestIdentityCleanupRemovesTheAccountAndBinding(t *testing.T) {
	rel := adapter.NewApplicationRelease(testApplication())
	service, c := newIdentity(t)

	if out := service.Ensure(context.Background(), rel); out.Err != nil {
		t.Fatalf("Ensure returned %v", out.Err)
	}
	if _, err := service.Cleanup(context.Background(), rel); err != nil {
		t.Fatalf("Cleanup returned %v", err)
	}

	names := rel.InternalNames()
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: names.ServiceAccount}, &corev1.ServiceAccount{}); !apierrors.IsNotFound(err) {
		t.Fatalf("service account must be gone, got %v", err)
	}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "team-a", Name: names.ServiceAccount}, &rbacv1.RoleBinding{}); !apierrors.IsNotFound(err) {
		t.Fatalf("role binding must be gone, got %v", err)
	}

	pending, err := service.Cleanup(context.Background(), rel)
	if err != nil {
		t.Fatalf("second Cleanup returned %v", err)
	}
	if pending != nil {
		t.Fatalf("nothing is left to wait for, got %T %s", pending, client.ObjectKeyFromObject(pending))
	}
}

func TestIdentityCleanupLeavesAForeignRoleBindingAlone(t *testing.T) {
	rel := adapter.NewApplicationRelease(testApplication())
	names := rel.InternalNames()
	foreign := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: names.ServiceAccount, Namespace: "team-a"},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: "someone-elses-role"},
		Subjects:   []rbacv1.Subject{{Kind: rbacv1.UserKind, Name: "someone-else"}},
	}
	service, c := newIdentity(t, foreign)

	pending, err := service.Cleanup(context.Background(), rel)
	if err != nil {
		t.Fatalf("Cleanup returned %v", err)
	}
	if pending != nil {
		t.Fatalf("a binding that is not ours must not be waited on, got %s", client.ObjectKeyFromObject(pending))
	}

	stored := &rbacv1.RoleBinding{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(foreign), stored); err != nil {
		t.Fatalf("a foreign role binding must survive cleanup: %v", err)
	}
	if stored.RoleRef != foreign.RoleRef {
		t.Fatalf("roleRef = %+v, want it untouched", stored.RoleRef)
	}
}

func TestIdentityCleanupWaitsForAnObjectHeldByAForeignFinalizer(t *testing.T) {
	const foreignFinalizer = "policy.example.com/protect"

	rel := adapter.NewApplicationRelease(testApplication())
	name := rel.InternalNames().ServiceAccount

	account := func(finalizers ...string) *corev1.ServiceAccount {
		return &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: testNamespace, Finalizers: finalizers,
		}}
	}
	binding := func(finalizers ...string) *rbacv1.RoleBinding {
		return &rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{
				Name: name, Namespace: "team-a", Labels: applicationRBACLabels(), Finalizers: finalizers,
			},
			RoleRef: applicationRoleRef(true),
		}
	}
	tests := []struct {
		name string
		held client.Object
		free client.Object
	}{
		{name: "the service account is held", held: account(foreignFinalizer), free: binding()},
		{name: "the role binding is held", held: binding(foreignFinalizer), free: account()},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			heldKey := client.ObjectKeyFromObject(tt.held)
			service, c := newIdentity(t, tt.held, tt.free)

			for pass := 1; pass <= 3; pass++ {
				pending, err := service.Cleanup(context.Background(), rel)
				if err != nil {
					t.Fatalf("pass %d: Cleanup returned %v", pass, err)
				}
				if pending == nil {
					t.Fatalf("pass %d: cleanup reported done while %s is still terminating", pass, heldKey)
				}
				if pass == 1 {
					continue
				}
				if got := client.ObjectKeyFromObject(pending); got != heldKey {
					t.Fatalf("pass %d: waiting on %s, want %s", pass, got, heldKey)
				}
				if pending.GetDeletionTimestamp().IsZero() {
					t.Fatalf("pass %d: the object waited on was never asked to go away", pass)
				}
			}

			if err := c.Get(context.Background(), heldKey, tt.held); err != nil {
				t.Fatalf("getting the held object: %v", err)
			}
			tt.held.SetFinalizers(nil)
			if err := c.Update(context.Background(), tt.held); err != nil {
				t.Fatalf("releasing the foreign finalizer: %v", err)
			}

			pending, err := service.Cleanup(context.Background(), rel)
			if err != nil {
				t.Fatalf("Cleanup returned %v once the object was gone", err)
			}
			if pending != nil {
				t.Fatalf("cleanup must be done once %s is gone, still waiting on %s",
					heldKey, client.ObjectKeyFromObject(pending))
			}
		})
	}
}
