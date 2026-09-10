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

package helmapplication

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
)

func maintainedApplication() *helmv1alpha1.HelmApplication {
	return &helmv1alpha1.HelmApplication{
		ObjectMeta: metav1.ObjectMeta{Name: "my-app", Namespace: "team-a"},
		Spec:       helmv1alpha1.HelmApplicationSpec{Maintenance: string(helmv1alpha1.NoResourceReconciliation)},
	}
}

func newValidator(t *testing.T, interceptors interceptor.Funcs, objects ...client.Object) *HelmApplicationWebhookValidator {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("registering client-go scheme: %v", err)
	}
	if err := helmv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("registering helm scheme: %v", err)
	}

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objects...).
		WithInterceptorFuncs(interceptors).
		Build()

	return &HelmApplicationWebhookValidator{Client: c}
}

func namespaceFixture(name string, terminating bool) *corev1.Namespace {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if terminating {
		now := metav1.Now()
		ns.DeletionTimestamp = &now
		// The fake client refuses an object carrying a deletion timestamp with no
		// finalizer holding it; a real terminating namespace always has one.
		ns.Finalizers = []string{"kubernetes"}
	}

	return ns
}

// TestValidateDeleteRefusesAMaintainedApplication is the rule itself: maintenance
// mode is what keeps an application from being deleted by mistake.
func TestValidateDeleteRefusesAMaintainedApplication(t *testing.T) {
	app := maintainedApplication()
	v := newValidator(t, interceptor.Funcs{}, namespaceFixture("team-a", false))

	if _, err := v.ValidateDelete(context.Background(), app); err == nil {
		t.Fatal("an application in maintenance must not be deletable")
	}
}

// TestValidateDeleteAllowsAnApplicationWhoseNamespaceIsGoing pins the exception.
// Namespace deletion deletes the namespace's objects one by one and waits for each
// of them, so a DELETE this webhook denies would leave the namespace Terminating
// with no way out.
func TestValidateDeleteAllowsAnApplicationWhoseNamespaceIsGoing(t *testing.T) {
	tests := []struct {
		name    string
		objects []client.Object
	}{
		{name: "namespace is terminating", objects: []client.Object{namespaceFixture("team-a", true)}},
		{name: "namespace is already gone", objects: nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := newValidator(t, interceptor.Funcs{}, tt.objects...)

			if _, err := v.ValidateDelete(context.Background(), maintainedApplication()); err != nil {
				t.Fatalf("the delete must be allowed, got %v", err)
			}
		})
	}
}

// TestValidateDeleteFailsClosedOnANamespaceReadError pins the direction of the
// doubt: an API error is not evidence that the namespace is going away, so the
// refusal stands rather than becoming a way past it.
func TestValidateDeleteFailsClosedOnANamespaceReadError(t *testing.T) {
	unreadable := interceptor.Funcs{
		Get: func(_ context.Context, _ client.WithWatch, _ client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
			return errors.New("apiserver is unavailable")
		},
	}
	v := newValidator(t, unreadable)

	if _, err := v.ValidateDelete(context.Background(), maintainedApplication()); err == nil {
		t.Fatal("the refusal must stand when the namespace cannot be read")
	}
}

// TestValidateDeleteAllowsAnUnmaintainedApplication is the ordinary case: nothing
// about the namespace is even read.
func TestValidateDeleteAllowsAnUnmaintainedApplication(t *testing.T) {
	app := &helmv1alpha1.HelmApplication{ObjectMeta: metav1.ObjectMeta{Name: "my-app", Namespace: "team-a"}}
	v := newValidator(t, interceptor.Funcs{})

	if _, err := v.ValidateDelete(context.Background(), app); err != nil {
		t.Fatalf("an application not in maintenance must be deletable, got %v", err)
	}
}
