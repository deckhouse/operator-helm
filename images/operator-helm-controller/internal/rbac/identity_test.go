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

	return NewIdentity(c, testNamespace), c
}

// TestIdentityEnsureCreatesTheIdentity pins the three objects and their shape: the
// account lives in the operator namespace without a token, the Role and the binding
// live in the application namespace, and the binding names the account by its
// operator-namespace identity — helm-controller impersonates
// system:serviceaccount:<operator namespace>:<name>.
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

	role := &rbacv1.Role{}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "team-a", Name: ApplicationRoleName}, role); err != nil {
		t.Fatalf("role was not created in the application namespace: %v", err)
	}
	wantRules := []rbacv1.PolicyRule{{APIGroups: []string{"*"}, Resources: []string{"*"}, Verbs: []string{"*"}}}
	if !reflect.DeepEqual(role.Rules, wantRules) {
		t.Fatalf("role rules = %+v, want full rights inside the namespace", role.Rules)
	}
	if !reflect.DeepEqual(role.Labels, applicationRBACLabels()) {
		t.Fatalf("role labels = %v, want %v", role.Labels, applicationRBACLabels())
	}

	binding := &rbacv1.RoleBinding{}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "team-a", Name: names.ServiceAccount}, binding); err != nil {
		t.Fatalf("role binding was not created in the application namespace: %v", err)
	}
	if binding.RoleRef != (rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: ApplicationRoleName}) {
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

// TestIdentityEnsureRewritesAnEditedRole pins that a Role of ours is reconciled rather
// than seeded: rules narrowed out of band are written back, and the managed-by label
// is kept. A label someone else put there survives, as on the account and the binding.
func TestIdentityEnsureRewritesAnEditedRole(t *testing.T) {
	rel := adapter.NewApplicationRelease(testApplication())
	edited := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ApplicationRoleName,
			Namespace: "team-a",
			Labels: map[string]string{
				helmv1alpha1.LabelManagedBy: helmv1alpha1.LabelManagedByValue,
				"cost-center":               "platform",
			},
		},
		Rules: []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"configmaps"}, Verbs: []string{"get"}}},
	}
	service, c := newIdentity(t, edited)

	if out := service.Ensure(context.Background(), rel); out.Err != nil {
		t.Fatalf("Ensure returned %v", out.Err)
	}

	role := &rbacv1.Role{}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "team-a", Name: ApplicationRoleName}, role); err != nil {
		t.Fatalf("getting role: %v", err)
	}
	wantRules := []rbacv1.PolicyRule{{APIGroups: []string{"*"}, Resources: []string{"*"}, Verbs: []string{"*"}}}
	if !reflect.DeepEqual(role.Rules, wantRules) {
		t.Fatalf("a narrowed role must be written back, rules = %+v", role.Rules)
	}
	for key, want := range applicationRBACLabels() {
		if role.Labels[key] != want {
			t.Fatalf("role label %q = %q, want %q", key, role.Labels[key], want)
		}
	}
	if role.Labels["cost-center"] != "platform" {
		t.Fatalf("role labels = %v, want the foreign label kept", role.Labels)
	}
}

// TestIdentityEnsureRefusesARoleThatIsNotOurs pins the ownership test on the shared
// Role. Its name is fixed, so a namespace owner can have put their own Role there;
// widening it to full rights and binding every application of the namespace to it
// is not a decision to make on their behalf. The verdict is terminal because the
// watch on the kind selects on the very label the object lacks: nothing observes it
// being removed either.
func TestIdentityEnsureRefusesARoleThatIsNotOurs(t *testing.T) {
	rel := adapter.NewApplicationRelease(testApplication())
	rules := []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"configmaps"}, Verbs: []string{"get"}}}
	foreign := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: ApplicationRoleName, Namespace: "team-a"},
		Rules:      rules,
	}
	service, c := newIdentity(t, foreign)

	out := service.Ensure(context.Background(), rel)
	if out.Err == nil {
		t.Fatal("a role that is not ours must be reported, not seized")
	}
	if !out.Terminal {
		t.Fatalf("outcome = %+v, want it terminal", out)
	}
	if out.Reason != helmv1alpha1.ReasonForeignRBACObject {
		t.Fatalf("reason = %q, want %q", out.Reason, helmv1alpha1.ReasonForeignRBACObject)
	}
	if !strings.Contains(out.Message, "team-a/"+ApplicationRoleName) {
		t.Fatalf("message %q must name the role", out.Message)
	}

	stored := &rbacv1.Role{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(foreign), stored); err != nil {
		t.Fatalf("the foreign role must survive: %v", err)
	}
	if !reflect.DeepEqual(stored.Rules, rules) {
		t.Fatalf("rules = %+v, want them untouched", stored.Rules)
	}
	if len(stored.Labels) != 0 {
		t.Fatalf("labels = %v, want the foreign role left unlabelled", stored.Labels)
	}
}

// TestIdentityEnsureRefusesABindingThatIsNotOurs pins the same ownership test on the
// binding. The derived name is fully computable by anyone, so a binding found under
// it may belong to someone else; adding our account to it would grant that account
// whatever the binding grants. The object is left exactly as it was — deleting a
// binding we did not create is not ours to do either.
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

// TestIdentityEnsureReplacesOurBindingThatNamesAnotherRole pins the other side of the
// ownership test: a binding carrying our label is ours to shape whatever state it is
// found in, and a roleRef naming some other role is drift like any other. roleRef is
// immutable, so putting it right means replacing the object rather than patching it.
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
	if stored.RoleRef != applicationRoleRef() {
		t.Fatalf("roleRef = %+v, want %+v", stored.RoleRef, applicationRoleRef())
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

// TestIdentityEnsureSurvivesLosingTheRoleCreateRace pins the one object of the three
// that two applications can race for: the Role is shared by the namespace, so the
// application that reads it as missing a moment too late is refused the create. It
// must reach the same end state, not report a failure.
func TestIdentityEnsureSurvivesLosingTheRoleCreateRace(t *testing.T) {
	rel := adapter.NewApplicationRelease(testApplication())

	var refused bool
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				role, ok := obj.(*rbacv1.Role)
				if !ok || refused {
					return cl.Create(ctx, obj, opts...)
				}

				// The winner of the race stores the Role; this pass learns of it the
				// way the API server would report it.
				refused = true
				if err := cl.Create(ctx, role.DeepCopy(), opts...); err != nil {
					return err
				}

				return apierrors.NewAlreadyExists(rbacv1.Resource("roles"), role.Name)
			},
		}).
		Build()
	service := NewIdentity(c, testNamespace)

	if out := service.Ensure(context.Background(), rel); out.Err != nil {
		t.Fatalf("Ensure returned %v", out.Err)
	}
	if !refused {
		t.Fatal("the test did not exercise the refused create")
	}

	role := &rbacv1.Role{}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "team-a", Name: ApplicationRoleName}, role); err != nil {
		t.Fatalf("getting role: %v", err)
	}
	if !reflect.DeepEqual(role.Labels, applicationRBACLabels()) {
		t.Fatalf("role labels = %v, want %v", role.Labels, applicationRBACLabels())
	}
}

// swapOnce replaces the stored object with replacement the first time the write
// under test reaches the client: it deletes what is there and creates the
// replacement, which is what an object taken over by someone else looks like — a
// new UID and a resource version of its own. It stands for the instant between the
// ownership test and the write, the one the optimistic lock exists to close.
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

// TestIdentityEnsureRefusesToPatchARoleSwappedAfterTheOwnershipTest pins that the
// ownership test and the write are one decision. The Role read carried our label;
// by the time the patch is sent someone else's Role holds the name, and a patch
// without the resource version it was read at would widen that Role to full rights.
// The conflict is not terminal: nothing about it says a retry cannot succeed, and
// the next pass judges what it then finds on its own labels.
func TestIdentityEnsureRefusesToPatchARoleSwappedAfterTheOwnershipTest(t *testing.T) {
	rel := adapter.NewApplicationRelease(testApplication())
	ours := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{
			Name: ApplicationRoleName, Namespace: "team-a", UID: "ours",
			Labels: applicationRBACLabels(),
		},
		// Narrowed out of band, so this pass has a patch to send at all.
		Rules: []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"configmaps"}, Verbs: []string{"get"}}},
	}
	theirRules := []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"secrets"}, Verbs: []string{"get"}}}
	theirs := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: ApplicationRoleName, Namespace: "team-a", UID: "theirs"},
		Rules:      theirRules,
	}

	swap, swapped := swapOnce(t, theirs)
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(ours).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				if _, ok := obj.(*rbacv1.Role); ok {
					swap(ctx, cl)
				}

				return cl.Patch(ctx, obj, patch, opts...)
			},
		}).
		Build()

	out := NewIdentity(c, testNamespace).Ensure(context.Background(), rel)
	if !*swapped {
		t.Fatal("the test did not exercise a patch of the role")
	}
	if !apierrors.IsConflict(out.Err) {
		t.Fatalf("Ensure returned %v, want the patch rejected as a conflict", out.Err)
	}
	if out.Terminal {
		t.Fatalf("outcome = %+v, want a conflict retried rather than stalled", out)
	}
	if out.Reason != helmv1alpha1.ReasonRBACSetupFailed {
		t.Fatalf("reason = %q, want %q", out.Reason, helmv1alpha1.ReasonRBACSetupFailed)
	}

	stored := &rbacv1.Role{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(theirs), stored); err != nil {
		t.Fatalf("getting the role that took the name: %v", err)
	}
	if !reflect.DeepEqual(stored.Rules, theirRules) {
		t.Fatalf("rules = %+v, want the swapped-in role untouched", stored.Rules)
	}
	if len(stored.Labels) != 0 {
		t.Fatalf("labels = %v, want the swapped-in role left unlabelled", stored.Labels)
	}
}

// TestIdentityEnsureRefusesToPatchARoleBindingSwappedAfterTheOwnershipTest is the
// same window on the binding: the one that was read was ours, the one the patch
// would land on is not, and merging our subject into it would hand our account
// whatever that binding grants.
func TestIdentityEnsureRefusesToPatchARoleBindingSwappedAfterTheOwnershipTest(t *testing.T) {
	rel := adapter.NewApplicationRelease(testApplication())
	name := rel.InternalNames().ServiceAccount
	ours := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "team-a", UID: "ours", Labels: applicationRBACLabels()},
		RoleRef:    applicationRoleRef(),
		// Drifted out of band, so this pass has a patch to send at all.
		Subjects: []rbacv1.Subject{{Kind: rbacv1.UserKind, Name: "drifted"}},
	}
	theirSubjects := []rbacv1.Subject{{Kind: rbacv1.UserKind, Name: "someone-else"}}
	theirs := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "team-a", UID: "theirs"},
		RoleRef:    applicationRoleRef(),
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

	out := NewIdentity(c, testNamespace).Ensure(context.Background(), rel)
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

// TestIdentityCleanupRefusesToDeleteARoleBindingSwappedAfterTheOwnershipTest is the
// delete side of the same window. Cleanup deletes only a binding carrying our
// label; a binding recreated by someone else in between carries none, and without
// the preconditions naming the object that was read it would be deleted on the
// strength of a label it never had.
func TestIdentityCleanupRefusesToDeleteARoleBindingSwappedAfterTheOwnershipTest(t *testing.T) {
	rel := adapter.NewApplicationRelease(testApplication())
	name := rel.InternalNames().ServiceAccount
	ours := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "team-a", UID: "ours", Labels: applicationRBACLabels()},
		RoleRef:    applicationRoleRef(),
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

	err := NewIdentity(c, testNamespace).Cleanup(context.Background(), rel)
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

func TestIdentityEnsureRecreatesADeletedRole(t *testing.T) {
	rel := adapter.NewApplicationRelease(testApplication())
	service, c := newIdentity(t)

	if out := service.Ensure(context.Background(), rel); out.Err != nil {
		t.Fatalf("first Ensure returned %v", out.Err)
	}
	if err := c.Delete(context.Background(), &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: ApplicationRoleName, Namespace: "team-a"}}); err != nil {
		t.Fatalf("deleting role: %v", err)
	}
	if out := service.Ensure(context.Background(), rel); out.Err != nil {
		t.Fatalf("second Ensure returned %v", out.Err)
	}

	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "team-a", Name: ApplicationRoleName}, &rbacv1.Role{}); err != nil {
		t.Fatalf("a deleted role must be recreated: %v", err)
	}
}

// TestIdentityEnsureKeepsForeignLabelsOnTheServiceAccountAndBinding pins that a label
// put there by someone else (a policy engine, a cost allocator) survives a
// reconcile: only the keys we own are kept authoritative, mirroring how
// applyHelmReleaseSpec and applyHelmChartSpec merge their labels.
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

// TestIdentityCleanupRemovesTheAccountAndBindingButKeepsTheRole pins spec 9.4: the
// account and the binding belong to one application; the Role belongs to the
// namespace and may have been edited by its owner, so it is never deleted.
func TestIdentityCleanupRemovesTheAccountAndBindingButKeepsTheRole(t *testing.T) {
	rel := adapter.NewApplicationRelease(testApplication())
	service, c := newIdentity(t)

	if out := service.Ensure(context.Background(), rel); out.Err != nil {
		t.Fatalf("Ensure returned %v", out.Err)
	}
	if err := service.Cleanup(context.Background(), rel); err != nil {
		t.Fatalf("Cleanup returned %v", err)
	}

	names := rel.InternalNames()
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: names.ServiceAccount}, &corev1.ServiceAccount{}); !apierrors.IsNotFound(err) {
		t.Fatalf("service account must be gone, got %v", err)
	}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "team-a", Name: names.ServiceAccount}, &rbacv1.RoleBinding{}); !apierrors.IsNotFound(err) {
		t.Fatalf("role binding must be gone, got %v", err)
	}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "team-a", Name: ApplicationRoleName}, &rbacv1.Role{}); err != nil {
		t.Fatalf("the role must survive the application: %v", err)
	}

	if err := service.Cleanup(context.Background(), rel); err != nil {
		t.Fatalf("second Cleanup returned %v", err)
	}
}

// TestIdentityCleanupLeavesAForeignRoleBindingAlone pins the delete-side mirror of
// TestIdentityEnsureRefusesToAdoptAForeignRoleBinding: the derived name is fully
// computable by anyone, so a binding found under it may belong to someone else.
// Such a binding is left alone, and that must not block the rest of the cleanup.
func TestIdentityCleanupLeavesAForeignRoleBindingAlone(t *testing.T) {
	rel := adapter.NewApplicationRelease(testApplication())
	names := rel.InternalNames()
	foreign := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: names.ServiceAccount, Namespace: "team-a"},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: "someone-elses-role"},
		Subjects:   []rbacv1.Subject{{Kind: rbacv1.UserKind, Name: "someone-else"}},
	}
	service, c := newIdentity(t, foreign)

	if err := service.Cleanup(context.Background(), rel); err != nil {
		t.Fatalf("Cleanup returned %v", err)
	}

	stored := &rbacv1.RoleBinding{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(foreign), stored); err != nil {
		t.Fatalf("a foreign role binding must survive cleanup: %v", err)
	}
	if stored.RoleRef != foreign.RoleRef {
		t.Fatalf("roleRef = %+v, want it untouched", stored.RoleRef)
	}
}
