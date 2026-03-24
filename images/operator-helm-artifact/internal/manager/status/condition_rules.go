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

package status

import (
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
)

// ErrorConditionRule defines how a specific child condition type should be
// treated as an error for the parent object.
type ErrorConditionRule struct {
	Type          string
	TriggerStatus metav1.ConditionStatus
	Reason        string
}

// ProcessChildConditions inspects a set of child conditions and returns a
// Status reflecting the aggregate state. Error rules are checked first (in
// order), then Reconciling, then Ready. If nothing matches, an Unknown status
// with ReasonReconciling is returned.
func ProcessChildConditions(
	conditions []metav1.Condition,
	generation int64,
	parentObj client.Object,
	errorRules []ErrorConditionRule,
) Status {
	for _, rule := range errorRules {
		cond := meta.FindStatusCondition(conditions, rule.Type)
		if cond != nil && cond.Status == rule.TriggerStatus {
			return Failed(parentObj, rule.Reason, cond.Message, nil)
		}
	}

	reconcilingCond := meta.FindStatusCondition(conditions, "Reconciling")
	if reconcilingCond != nil && reconcilingCond.Status == metav1.ConditionTrue {
		return Unknown(parentObj, helmv1alpha1.ReasonReconciling)
	}

	cond, observed := IsConditionObserved(conditions, helmv1alpha1.ConditionTypeReady, generation)
	if observed {
		return Status{
			Observed:           true,
			Status:             cond.Status,
			ObservedGeneration: parentObj.GetGeneration(),
			Reason:             cond.Reason,
			Message:            cond.Message,
		}
	}

	return Unknown(parentObj, helmv1alpha1.ReasonReconciling)
}
