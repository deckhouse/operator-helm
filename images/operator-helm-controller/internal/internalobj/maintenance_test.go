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

package internalobj

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
	"github.com/deckhouse/operator-helm/internal/adapter"
)

func TestChangeRequiredComparesTheRequestAgainstWhatWasCarriedOut(t *testing.T) {
	for _, tc := range []struct {
		name      string
		requested bool
		recorded  *metav1.ConditionStatus
		want      bool
	}{
		{name: "nothing requested and nothing recorded", requested: false, recorded: nil, want: false},
		{name: "requested and not yet recorded", requested: true, recorded: nil, want: true},
		{name: "requested and already recorded", requested: true, recorded: ptr.To(metav1.ConditionFalse), want: false},
		{name: "withdrawn while still recorded", requested: false, recorded: ptr.To(metav1.ConditionFalse), want: true},
		{name: "withdrawn and already lifted", requested: false, recorded: ptr.To(metav1.ConditionTrue), want: false},
		{name: "nothing requested and the condition only seeded", requested: false, recorded: ptr.To(metav1.ConditionUnknown), want: false},
		{name: "requested and the condition only seeded", requested: true, recorded: ptr.To(metav1.ConditionUnknown), want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			addon := &helmv1alpha1.HelmClusterAddon{ObjectMeta: metav1.ObjectMeta{Name: "metrics"}}
			if tc.requested {
				addon.Spec.Maintenance = string(helmv1alpha1.NoResourceReconciliation)
			}
			if tc.recorded != nil {
				addon.Status.Conditions = []metav1.Condition{{
					Type:   helmv1alpha1.ConditionTypeManaged,
					Status: *tc.recorded,
					Reason: helmv1alpha1.ReasonMaintenanceModeActive,
				}}
			}

			if got := NewMaintenance(nil, helmv1alpha1.TargetNamespace).ChangeRequired(adapter.NewAddonRelease(addon)); got != tc.want {
				t.Fatalf("ChangeRequired = %v, want %v", got, tc.want)
			}
		})
	}
}
