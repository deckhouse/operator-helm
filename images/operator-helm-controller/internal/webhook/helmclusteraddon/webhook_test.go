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
		// The same index function the manager registers, so the lookup the check
		// performs is the one it performs in the cluster.
		WithIndex(&helmv1alpha1.HelmClusterAddon{}, index.AddonChart, index.AddonChartIndexer).
		Build()

	return &HelmClusterAddonWebhookValidator{
		Client:       c,
		claimService: claim.NewClaims(c, c, helmv1alpha1.TargetNamespace),
	}
}

// addonUsing is an addon on an explicit repository/chart pair; the fixture above
// puts every addon on stable/redis, which is exactly what uniqueness forbids.
func addonUsing(name, repository, chart string) *helmv1alpha1.HelmClusterAddon {
	addon := addonFixture(name)
	addon.Spec.Chart.HelmClusterAddonRepository = repository
	addon.Spec.Chart.HelmClusterAddonChartName = chart

	return addon
}

// claimFixture is the Lease the reconciler creates for a pair: the webhook reads it
// to recognize the addon that already owns the pair.
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

// TestValidateCreateRejectsANameTooLongForTheSourceLabel pins the one place the
// bound belongs: the name goes on every internal object as a label value, which
// stops at 63 characters. CREATE only — a rule evaluated on every write would also
// block the status updates and the finalizer removal of an addon that already
// carries a longer name, leaving it undeletable.
func TestValidateCreateRejectsANameTooLongForTheSourceLabel(t *testing.T) {
	tests := []struct {
		name      string
		addonName string
		wantErr   bool
	}{
		{name: "at the limit", addonName: strings.Repeat("a", 63), wantErr: false},
		{name: "one over the limit", addonName: strings.Repeat("a", 64), wantErr: true},
		{name: "far over the limit", addonName: strings.Repeat("a", 200), wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := newValidator(t)

			_, err := v.ValidateCreate(context.Background(), addonFixture(tt.addonName))
			if tt.wantErr && err == nil {
				t.Fatalf("a %d-character name must be rejected", len(tt.addonName))
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("a %d-character name must be allowed, got %v", len(tt.addonName), err)
			}
		})
	}
}

// TestValidateCreateExplainsWhyTheNameIsBounded keeps the rejection actionable: the
// limit only makes sense together with the label that imposes it.
func TestValidateCreateExplainsWhyTheNameIsBounded(t *testing.T) {
	v := newValidator(t)

	_, err := v.ValidateCreate(context.Background(), addonFixture(strings.Repeat("a", 64)))
	if err == nil {
		t.Fatal("an over-long name must be rejected")
	}
	if !strings.Contains(err.Error(), helmv1alpha1.HelmClusterAddonLabelSourceName) {
		t.Fatalf("the error must name the label that bounds the name, got %v", err)
	}
}

// TestValidateUpdateAllowsAnExistingOverlongName is the reason the check is not a
// CEL rule on the type: an addon created before the bound existed must still be
// able to report status and to shed its cleanup finalizer, or it becomes
// undeletable.
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

// TestValidateDeleteAllowsAnExistingOverlongName is the other half of the same
// reason: the DELETE itself must go through.
func TestValidateDeleteAllowsAnExistingOverlongName(t *testing.T) {
	addon := addonFixture(strings.Repeat("a", 120))
	v := newValidator(t, addon)

	if _, err := v.ValidateDelete(context.Background(), addon); err != nil {
		t.Fatalf("an existing addon must stay deletable, got %v", err)
	}
}

// TestValidateCreateRejectsAChartAlreadyUsedByAnotherAddon is the rule the webhook
// exists for: one repository/chart pair, one addon. The check is advisory — two
// concurrent creates can both pass it, and the Lease in internal/claim is what
// actually settles the race — so what is pinned here is that an already visible
// duplicate is refused with the name of the addon holding the pair.
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

// TestValidateAllowsTheAddonThatHoldsTheClaim covers the two ways an addon is not
// its own duplicate: it is the object listed under the pair, or the claim Lease
// already records it as the holder — the state a re-created addon whose Lease
// outlived it comes back to.
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

// TestValidateUpdateSkipsTheCheckOnlyWhenForceReconcileAloneChanged pins the one
// exemption: annotating an addon to force a reconcile must go through even while it
// is the losing duplicate of a pair, or a conflict could never be re-driven. A write
// that also moves the chart reference is a new claim and is checked again.
func TestValidateUpdateSkipsTheCheckOnlyWhenForceReconcileAloneChanged(t *testing.T) {
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
			name: "nothing about the annotation changed",
			mutate: func(addon *helmv1alpha1.HelmClusterAddon) {
				addon.Spec.Values = &apiextensionsv1.JSON{Raw: []byte(`{"a":1}`)}
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			oldAddon := addonUsing("newcomer", "stable", "redis")
			newAddon := oldAddon.DeepCopy()
			tt.mutate(newAddon)

			// Both pairs are taken by an incumbent, so any check that runs refuses.
			v := newValidator(t,
				addonUsing("incumbent", "stable", "redis"),
				addonUsing("other-incumbent", "stable", "postgres"),
				oldAddon,
			)

			_, err := v.ValidateUpdate(context.Background(), oldAddon, newAddon)
			if tt.wantErr && err == nil {
				t.Fatal("the uniqueness check must run on this update")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("forcing a reconcile must stay possible, got %v", err)
			}
		})
	}
}

// TestUniquenessIsBypassedForTheControllerItself pins the exemption the controller
// needs: its own writes — status, finalizers, the chart reference it normalizes —
// must not be refused because the addon is a known duplicate, or a conflicting
// addon could not even record the conflict. Every other user is checked.
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
			v := newValidator(t, addonUsing("incumbent", "stable", "redis"))

			_, err := v.ValidateCreate(tt.ctx, addonUsing("newcomer", "stable", "redis"))
			if tt.wantErr && err == nil {
				t.Fatal("the duplicate must be refused for this user")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("the controller's own write must pass, got %v", err)
			}
		})
	}
}

// TestUniquenessFailsClosedWhenItCannotBeChecked pins that neither read is allowed
// to be skipped silently: an unreadable claim or an unreadable list must refuse the
// write rather than admit an unchecked duplicate.
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

// TestValidateRejectsASystemTargetNamespace keeps an addon from installing a chart
// into the platform's own namespaces, on both operations: the field is mutable, so
// CREATE alone would not hold the rule.
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

// TestValidateDeleteRefusesAMaintainedAddon pins what maintenance mode buys: an
// addon under it is not deleted by mistake, and the release it installed stays.
func TestValidateDeleteRefusesAMaintainedAddon(t *testing.T) {
	addon := addonFixture("maintained")
	addon.Spec.Maintenance = string(helmv1alpha1.NoResourceReconciliation)

	v := newValidator(t, addon)

	if _, err := v.ValidateDelete(context.Background(), addon); err == nil {
		t.Fatal("an addon in maintenance must not be deletable")
	}
}
