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
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
	"github.com/deckhouse/operator-helm/internal/adapter"
	"github.com/deckhouse/operator-helm/internal/source"
)

func testApplication() *helmv1alpha1.HelmApplication {
	return &helmv1alpha1.HelmApplication{
		ObjectMeta: metav1.ObjectMeta{Name: "my-app", Namespace: "team-a", Generation: 1},
		Spec: helmv1alpha1.HelmApplicationSpec{
			Chart: helmv1alpha1.HelmApplicationChartRef{Name: "podinfo", Repository: "stable", Version: "6.7.1"},
		},
	}
}

func newAccessService(t *testing.T, objects ...client.Object) (*AccessService, client.Client) {
	t.Helper()

	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objects...).Build()

	return NewAccessService(c, testNamespace), c
}

// TestEnsureAccessCreatesTheIdentity pins the three objects and their shape: the
// account lives in the operator namespace without a token, the Role and the binding
// live in the application namespace, and the binding names the account by its
// operator-namespace identity — helm-controller impersonates
// system:serviceaccount:<operator namespace>:<name>.
func TestEnsureAccessCreatesTheIdentity(t *testing.T) {
	rel := adapter.NewApplicationRelease(testApplication())
	service, c := newAccessService(t)

	if err := service.EnsureAccess(context.Background(), rel); err != nil {
		t.Fatalf("EnsureAccess returned %v", err)
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
}

// TestEnsureAccessLeavesAnEditedRoleAlone pins the seed rule: the Role is the place
// where a namespace owner may cut the rights down, so an existing Role is never
// read, compared or rewritten — only a missing one is created.
func TestEnsureAccessLeavesAnEditedRoleAlone(t *testing.T) {
	rel := adapter.NewApplicationRelease(testApplication())
	edited := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: ApplicationRoleName, Namespace: "team-a"},
		Rules:      []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"configmaps"}, Verbs: []string{"get"}}},
	}
	service, c := newAccessService(t, edited)

	if err := service.EnsureAccess(context.Background(), rel); err != nil {
		t.Fatalf("EnsureAccess returned %v", err)
	}

	role := &rbacv1.Role{}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "team-a", Name: ApplicationRoleName}, role); err != nil {
		t.Fatalf("getting role: %v", err)
	}
	if !reflect.DeepEqual(role.Rules, edited.Rules) {
		t.Fatalf("an existing role must not be rewritten, rules = %+v", role.Rules)
	}
}

// TestEnsureAccessRefusesToAdoptAForeignRoleBinding pins that a binding already
// living under our name but pointing at someone else's role is reported, not
// reused: adding our account as its subject would silently grant that account
// whatever that role grants. The object is left exactly as it was — deleting a
// binding we did not create is not ours to do either.
func TestEnsureAccessRefusesToAdoptAForeignRoleBinding(t *testing.T) {
	rel := adapter.NewApplicationRelease(testApplication())
	foreign := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: rel.InternalNames().ServiceAccount, Namespace: "team-a"},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: "someone-elses-role"},
		Subjects:   []rbacv1.Subject{{Kind: rbacv1.UserKind, Name: "someone-else"}},
	}
	service, c := newAccessService(t, foreign)

	err := service.EnsureAccess(context.Background(), rel)
	if err == nil {
		t.Fatal("a binding pointing at a foreign role must be reported, not adopted")
	}
	if !strings.Contains(err.Error(), "someone-elses-role") {
		t.Fatalf("error %q must name the existing roleRef", err.Error())
	}

	stored := &rbacv1.RoleBinding{}
	if getErr := c.Get(context.Background(), client.ObjectKeyFromObject(foreign), stored); getErr != nil {
		t.Fatalf("the foreign binding must survive: %v", getErr)
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

func TestEnsureAccessRecreatesADeletedRole(t *testing.T) {
	rel := adapter.NewApplicationRelease(testApplication())
	service, c := newAccessService(t)

	if err := service.EnsureAccess(context.Background(), rel); err != nil {
		t.Fatalf("first EnsureAccess returned %v", err)
	}
	if err := c.Delete(context.Background(), &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: ApplicationRoleName, Namespace: "team-a"}}); err != nil {
		t.Fatalf("deleting role: %v", err)
	}
	if err := service.EnsureAccess(context.Background(), rel); err != nil {
		t.Fatalf("second EnsureAccess returned %v", err)
	}

	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "team-a", Name: ApplicationRoleName}, &rbacv1.Role{}); err != nil {
		t.Fatalf("a deleted role must be recreated: %v", err)
	}
}

// TestEnsureAccessKeepsForeignLabelsOnTheServiceAccountAndBinding pins that a label
// put there by someone else (a policy engine, a cost allocator) survives a
// reconcile: only the keys we own are kept authoritative, mirroring how
// applyHelmReleaseSpec and applyHelmChartSpec merge their labels.
func TestEnsureAccessKeepsForeignLabelsOnTheServiceAccountAndBinding(t *testing.T) {
	rel := adapter.NewApplicationRelease(testApplication())
	names := rel.InternalNames()
	service, c := newAccessService(t)

	if err := service.EnsureAccess(context.Background(), rel); err != nil {
		t.Fatalf("first EnsureAccess returned %v", err)
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

	if err := service.EnsureAccess(context.Background(), rel); err != nil {
		t.Fatalf("second EnsureAccess returned %v", err)
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

func TestEnsureAccessIsANoopForAFamilyWithoutAServiceAccount(t *testing.T) {
	service, c := newAccessService(t)

	if err := service.EnsureAccess(context.Background(), adapter.NewAddonRelease(testAddon())); err != nil {
		t.Fatalf("EnsureAccess returned %v", err)
	}

	var accounts corev1.ServiceAccountList
	if err := c.List(context.Background(), &accounts); err != nil {
		t.Fatalf("listing service accounts: %v", err)
	}
	if len(accounts.Items) != 0 {
		t.Fatalf("no identity must be created for a release without a service account name, got %v", accounts.Items)
	}
}

// TestCleanupAccessRemovesTheAccountAndBindingButKeepsTheRole pins spec 9.4: the
// account and the binding belong to one application; the Role belongs to the
// namespace and may have been edited by its owner, so it is never deleted.
func TestCleanupAccessRemovesTheAccountAndBindingButKeepsTheRole(t *testing.T) {
	rel := adapter.NewApplicationRelease(testApplication())
	service, c := newAccessService(t)

	if err := service.EnsureAccess(context.Background(), rel); err != nil {
		t.Fatalf("EnsureAccess returned %v", err)
	}
	if err := service.CleanupAccess(context.Background(), rel); err != nil {
		t.Fatalf("CleanupAccess returned %v", err)
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

	// Cleaning up twice is fine: nothing is left to delete.
	if err := service.CleanupAccess(context.Background(), rel); err != nil {
		t.Fatalf("second CleanupAccess returned %v", err)
	}

	var _ source.AccessManager = service
}

// TestCleanupAccessLeavesAForeignRoleBindingAlone pins the delete-side mirror of
// TestEnsureAccessRefusesToAdoptAForeignRoleBinding: the derived name is fully
// computable by anyone, so a binding found under it may belong to someone else.
// Such a binding is left alone, and that must not block the rest of the cleanup.
func TestCleanupAccessLeavesAForeignRoleBindingAlone(t *testing.T) {
	rel := adapter.NewApplicationRelease(testApplication())
	names := rel.InternalNames()
	foreign := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: names.ServiceAccount, Namespace: "team-a"},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: "someone-elses-role"},
		Subjects:   []rbacv1.Subject{{Kind: rbacv1.UserKind, Name: "someone-else"}},
	}
	service, c := newAccessService(t, foreign)

	if err := service.CleanupAccess(context.Background(), rel); err != nil {
		t.Fatalf("CleanupAccess returned %v", err)
	}

	stored := &rbacv1.RoleBinding{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(foreign), stored); err != nil {
		t.Fatalf("a foreign role binding must survive cleanup: %v", err)
	}
	if stored.RoleRef != foreign.RoleRef {
		t.Fatalf("roleRef = %+v, want it untouched", stored.RoleRef)
	}
}
