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

package helmclusteraddon

import (
	"context"
	"errors"
	"strings"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
	"github.com/deckhouse/operator-helm/internal/claim"
	"github.com/deckhouse/operator-helm/internal/index"
	"github.com/deckhouse/operator-helm/internal/utils"
)

func addonFixture(name string) *helmv1alpha1.HelmClusterAddon {
	return &helmv1alpha1.HelmClusterAddon{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: helmv1alpha1.HelmClusterAddonSpec{
			Namespace: "team-a",
			Chart: helmv1alpha1.HelmClusterAddonChartRef{
				HelmClusterAddonRepository: "stable",
				HelmClusterAddonChartName:  "redis",
			},
		},
	}
}

func newValidator(t *testing.T, objects ...client.Object) *HelmClusterAddonWebhookValidator {
	t.Helper()

	return newValidatorWithFaults(t, interceptor.Funcs{}, objects...)
}

func newValidatorWithFaults(t *testing.T, faults interceptor.Funcs, objects ...client.Object) *HelmClusterAddonWebhookValidator {
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
		WithInterceptorFuncs(faults).
		WithIndex(&helmv1alpha1.HelmClusterAddon{}, index.AddonChart, index.AddonChartIndexer).
		Build()

	return &HelmClusterAddonWebhookValidator{
		Client:       c,
		claimService: claim.NewClaims(c, c, helmv1alpha1.TargetNamespace),
	}
}

func addonUsing(name, repository, chart string) *helmv1alpha1.HelmClusterAddon {
	addon := addonFixture(name)
	addon.Spec.Chart.HelmClusterAddonRepository = repository
	addon.Spec.Chart.HelmClusterAddonChartName = chart

	return addon
}

func claimFixture(repository, chart, holder string) *coordinationv1.Lease {
	return &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{
			Name:      utils.GetChartClaimLeaseName(repository, chart),
			Namespace: helmv1alpha1.TargetNamespace,
		},
		Spec: coordinationv1.LeaseSpec{HolderIdentity: &holder},
	}
}

func requestBy(username string) context.Context {
	return admission.NewContextWithRequest(context.Background(), admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{UserInfo: authenticationv1.UserInfo{Username: username}},
	})
}

func TestValidateUpdateAllowsAnExistingOverlongName(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(addon *helmv1alpha1.HelmClusterAddon)
	}{
		{
			name: "reporting status",
			mutate: func(addon *helmv1alpha1.HelmClusterAddon) {
				addon.Status.ObservedGeneration = addon.Generation
				addon.Status.Conditions = []metav1.Condition{{
					Type:               helmv1alpha1.ConditionTypeReady,
					Status:             metav1.ConditionTrue,
					Reason:             helmv1alpha1.ReasonSuccess,
					LastTransitionTime: metav1.Now(),
				}}
			},
		},
		{
			name: "shedding the cleanup finalizer",
			mutate: func(addon *helmv1alpha1.HelmClusterAddon) {
				addon.Finalizers = nil
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			oldAddon := addonFixture(strings.Repeat("a", 120))
			oldAddon.Generation = 1
			oldAddon.Finalizers = []string{helmv1alpha1.FinalizerName}

			newAddon := oldAddon.DeepCopy()
			tt.mutate(newAddon)

			v := newValidator(t, oldAddon)

			if _, err := v.ValidateUpdate(context.Background(), oldAddon, newAddon); err != nil {
				t.Fatalf("an existing addon must stay updatable, got %v", err)
			}
		})
	}
}

func TestValidateDeleteAllowsAnExistingOverlongName(t *testing.T) {
	addon := addonFixture(strings.Repeat("a", 120))
	v := newValidator(t, addon)

	if _, err := v.ValidateDelete(context.Background(), addon); err != nil {
		t.Fatalf("an existing addon must stay deletable, got %v", err)
	}
}

func TestValidateCreateRejectsAChartAlreadyUsedByAnotherAddon(t *testing.T) {
	tests := []struct {
		name     string
		existing []client.Object
		created  *helmv1alpha1.HelmClusterAddon
		wantErr  bool
	}{
		{
			name:     "the same pair",
			existing: []client.Object{addonUsing("incumbent", "stable", "redis")},
			created:  addonUsing("newcomer", "stable", "redis"),
			wantErr:  true,
		},
		{
			name:     "another chart in the same repository",
			existing: []client.Object{addonUsing("incumbent", "stable", "redis")},
			created:  addonUsing("newcomer", "stable", "postgres"),
		},
		{
			name:     "the same chart in another repository",
			existing: []client.Object{addonUsing("incumbent", "stable", "redis")},
			created:  addonUsing("newcomer", "testing", "redis"),
		},
		{
			name:    "nothing uses the pair yet",
			created: addonUsing("newcomer", "stable", "redis"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := newValidator(t, tt.existing...)

			_, err := v.ValidateCreate(context.Background(), tt.created)
			if tt.wantErr {
				if err == nil {
					t.Fatal("the pair is already used and must be refused")
				}
				if !strings.Contains(err.Error(), "incumbent") {
					t.Fatalf("the error must name the addon holding the pair, got %v", err)
				}

				return
			}
			if err != nil {
				t.Fatalf("the pair is free and must be allowed, got %v", err)
			}
		})
	}
}

func TestValidateAllowsTheAddonThatHoldsTheClaim(t *testing.T) {
	t.Run("the listed object is the addon itself", func(t *testing.T) {
		addon := addonUsing("mine", "stable", "redis")
		v := newValidator(t, addon)

		if _, err := v.ValidateUpdate(context.Background(), addon, addon.DeepCopy()); err != nil {
			t.Fatalf("an addon must not conflict with itself, got %v", err)
		}
	})

	t.Run("the claim lease names the addon", func(t *testing.T) {
		v := newValidator(t,
			addonUsing("leftover", "stable", "redis"),
			claimFixture("stable", "redis", "mine"),
		)

		if _, err := v.ValidateCreate(context.Background(), addonUsing("mine", "stable", "redis")); err != nil {
			t.Fatalf("the claim holder must be allowed back, got %v", err)
		}
	})
}

func TestValidateUpdateChecksUniquenessOnlyWhenTheChartReferenceChanged(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(addon *helmv1alpha1.HelmClusterAddon)
		wantErr bool
	}{
		{
			name: "only the force-reconcile annotation changed",
			mutate: func(addon *helmv1alpha1.HelmClusterAddon) {
				addon.Annotations = map[string]string{helmv1alpha1.AnnotationForceReconcile: "now"}
			},
		},
		{
			name: "the annotation changed together with the chart reference",
			mutate: func(addon *helmv1alpha1.HelmClusterAddon) {
				addon.Annotations = map[string]string{helmv1alpha1.AnnotationForceReconcile: "now"}
				addon.Spec.Chart.HelmClusterAddonChartName = "postgres"
			},
			wantErr: true,
		},
		{
			name: "the annotation changed together with the repository reference",
			mutate: func(addon *helmv1alpha1.HelmClusterAddon) {
				addon.Annotations = map[string]string{helmv1alpha1.AnnotationForceReconcile: "now"}
				addon.Spec.Chart.HelmClusterAddonRepository = "testing"
			},
			wantErr: true,
		},
		{
			name: "only the values changed",
			mutate: func(addon *helmv1alpha1.HelmClusterAddon) {
				addon.Spec.Values = &apiextensionsv1.JSON{Raw: []byte(`{"a":1}`)}
			},
		},
		{
			name: "the chart reference changed alone",
			mutate: func(addon *helmv1alpha1.HelmClusterAddon) {
				addon.Spec.Chart.HelmClusterAddonChartName = "postgres"
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			oldAddon := addonUsing("newcomer", "stable", "redis")
			newAddon := oldAddon.DeepCopy()
			tt.mutate(newAddon)

			v := newValidator(t,
				addonUsing("incumbent", "stable", "redis"),
				addonUsing("other-incumbent", "stable", "postgres"),
				addonUsing("third-incumbent", "testing", "redis"),
				oldAddon,
			)

			_, err := v.ValidateUpdate(context.Background(), oldAddon, newAddon)
			if tt.wantErr && err == nil {
				t.Fatal("the uniqueness check must run on this update")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("an update that keeps the chart reference must not be checked, got %v", err)
			}
		})
	}
}

func TestUniquenessIsBypassedForTheControllerItself(t *testing.T) {
	tests := []struct {
		name    string
		ctx     context.Context
		wantErr bool
	}{
		{name: "the operator's service account", ctx: requestBy("system:serviceaccount:d8-operator-helm:operator-helm-controller")},
		{name: "another service account in the same namespace", ctx: requestBy("system:serviceaccount:d8-operator-helm:someone-else"), wantErr: true},
		{name: "an ordinary user", ctx: requestBy("system:admin"), wantErr: true},
		{name: "no admission request in the context", ctx: context.Background(), wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Run("on create", func(t *testing.T) {
				v := newValidator(t, addonUsing("incumbent", "stable", "redis"))

				_, err := v.ValidateCreate(tt.ctx, addonUsing("newcomer", "stable", "redis"))
				if tt.wantErr && err == nil {
					t.Fatal("the duplicate must be refused for this user")
				}
				if !tt.wantErr && err != nil {
					t.Fatalf("the controller's own write must pass, got %v", err)
				}
			})

			t.Run("on update", func(t *testing.T) {
				oldAddon := addonUsing("newcomer", "stable", "postgres")
				newAddon := addonUsing("newcomer", "stable", "redis")
				v := newValidator(t, addonUsing("incumbent", "stable", "redis"), oldAddon)

				_, err := v.ValidateUpdate(tt.ctx, oldAddon, newAddon)
				if tt.wantErr && err == nil {
					t.Fatal("the duplicate must be refused for this user")
				}
				if !tt.wantErr && err != nil {
					t.Fatalf("the controller's own write must pass, got %v", err)
				}
			})
		})
	}
}

func TestUniquenessFailsClosedWhenItCannotBeChecked(t *testing.T) {
	tests := []struct {
		name   string
		faults interceptor.Funcs
	}{
		{
			name: "the claim lease cannot be read",
			faults: interceptor.Funcs{
				Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
					return errors.New("api server is unavailable")
				},
			},
		},
		{
			name: "the addons cannot be listed",
			faults: interceptor.Funcs{
				List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
					return errors.New("api server is unavailable")
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := newValidatorWithFaults(t, tt.faults)

			if _, err := v.ValidateCreate(context.Background(), addonUsing("newcomer", "stable", "redis")); err == nil {
				t.Fatal("an unverifiable write must be refused")
			}
		})
	}
}

func TestValidateRejectsASystemTargetNamespace(t *testing.T) {
	tests := []struct {
		name      string
		namespace string
		wantErr   bool
	}{
		{name: "kube-system", namespace: "kube-system", wantErr: true},
		{name: "a deckhouse namespace", namespace: "d8-monitoring", wantErr: true},
		{name: "an ordinary namespace", namespace: "team-a"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			addon := addonUsing("newcomer", "stable", "redis")
			addon.Spec.Namespace = tt.namespace
			v := newValidator(t)

			_, createErr := v.ValidateCreate(context.Background(), addon)
			_, updateErr := v.ValidateUpdate(context.Background(), addonUsing("newcomer", "stable", "redis"), addon)

			for op, err := range map[string]error{"create": createErr, "update": updateErr} {
				if tt.wantErr && err == nil {
					t.Fatalf("%s into namespace %q must be rejected", op, tt.namespace)
				}
				if !tt.wantErr && err != nil {
					t.Fatalf("%s into namespace %q must be allowed, got %v", op, tt.namespace, err)
				}
			}
		})
	}
}

func TestValidateDeleteRefusesAMaintainedAddon(t *testing.T) {
	addon := addonFixture("maintained")
	addon.Spec.Maintenance = string(helmv1alpha1.NoResourceReconciliation)

	v := newValidator(t, addon)

	if _, err := v.ValidateDelete(context.Background(), addon); err == nil {
		t.Fatal("an addon in maintenance must not be deletable")
	}
}
