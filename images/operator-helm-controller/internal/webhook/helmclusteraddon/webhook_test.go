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
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
	"github.com/deckhouse/operator-helm/internal/claim"
	"github.com/deckhouse/operator-helm/internal/index"
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
		WithIndex(&helmv1alpha1.HelmClusterAddon{}, index.AddonChart, func(obj client.Object) []string {
			addon := obj.(*helmv1alpha1.HelmClusterAddon)

			return []string{index.AddonChartValue(
				addon.Spec.Chart.HelmClusterAddonRepository,
				addon.Spec.Chart.HelmClusterAddonChartName,
			)}
		}).
		Build()

	return &HelmClusterAddonWebhookValidator{
		Client:       c,
		claimService: claim.NewClaims(c, c, helmv1alpha1.TargetNamespace),
	}
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
