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
	"regexp"
	"slices"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
)

// InternalObjectState describes the observed state of one internal FluxCD object a release
// owns. Observed is false until the object reports a verdict for the spec it was given.
type InternalObjectState struct {
	Observed bool
	// Stalled reports that the internal object gave up on the spec it was given: it
	// exhausted its remediation attempts, or reached a fault it will not retry.
	Stalled bool
	Status  metav1.ConditionStatus
	Reason  string
	Message string
}

// Ready reports the state every consumer means by it: the object has observed the
// spec it was given and calls itself ready for it.
func (s InternalObjectState) Ready() bool {
	return s.Observed && s.Status == metav1.ConditionTrue
}

// ErrorConditionRule maps a condition an internal FluxCD object raises about itself to
// the reason the release reports because of it. The rules of one kind are checked in order.
type ErrorConditionRule struct {
	Type          string
	TriggerStatus metav1.ConditionStatus
	Reason        string
}

// TerminalMessageRule recognises, by the message it is reported with, a fault no retry
// resolves while the internal object keeps retrying it and calling itself progressing.
// ConditionTypes are the conditions the message may be found in.
type TerminalMessageRule struct {
	ConditionTypes []string
	Pattern        *regexp.Regexp
	Reason         string
}

var terminalMessageRules = []TerminalMessageRule{
	{
		ConditionTypes: []string{helmv1alpha1.ConditionTypeReady, "Released", "Remediated"},
		Pattern:        regexp.MustCompile(`is forbidden: User "[^"]+" cannot [a-z]+ resource "[^"]+" in API group "[^"]*"`),
		Reason:         helmv1alpha1.ReasonAccessDenied,
	},
	{
		ConditionTypes: []string{helmv1alpha1.ConditionTypeReady, "Released", "Remediated"},
		Pattern:        regexp.MustCompile(`error while running post render on files`),
		Reason:         helmv1alpha1.ReasonRenderFailed,
	},
}

const prerequisitesMessage = "Fulfilling prerequisites"

var upstreamFailureReasons = map[string]string{
	"ArtifactFailed":  helmv1alpha1.ReasonChartFetchFailed,
	"SourceNotReady":  helmv1alpha1.ReasonChartSourceNotReady,
	"ValuesError":     helmv1alpha1.ReasonInvalidValues,
	"RetriesExceeded": helmv1alpha1.ReasonRetriesExceeded,
}

func reduceInternalConditions(
	conditions []metav1.Condition,
	generation int64,
	errorRules []ErrorConditionRule,
) InternalObjectState {
	// Checked first: such a fault is reported under a Reconciling the object never lifts.
	if state, ok := matchTerminalMessage(conditions, generation); ok {
		return state
	}

	stalled := currentCondition(conditions, helmv1alpha1.ConditionTypeStalled, generation)
	gaveUp := stalled != nil && stalled.Status == metav1.ConditionTrue

	reconciling := InternalObjectState{
		Status: metav1.ConditionUnknown,
		Reason: helmv1alpha1.ReasonReconciling,
	}

	// Order matters: having given up outranks a Reconciling condition the attempt that gave up left behind.
	if !gaveUp {
		cond := currentCondition(conditions, helmv1alpha1.ConditionTypeReconciling, generation)
		if cond != nil && cond.Status == metav1.ConditionTrue && cond.Reason != helmv1alpha1.ReasonProgressingWithRetry {
			ready, observed := conditionObserved(conditions, helmv1alpha1.ConditionTypeReady, generation)
			if !observed || ready.Status != metav1.ConditionFalse || cond.Message != prerequisitesMessage {
				return reconciling
			}

			summarized := slices.ContainsFunc(errorRules, func(rule ErrorConditionRule) bool {
				source := currentCondition(conditions, rule.Type, generation)
				return source != nil && source.Reason == ready.Reason && source.Message == ready.Message
			})
			if !summarized {
				return ownState(ready.Status, ready, false)
			}
		}
	}

	for _, rule := range errorRules {
		if cond := currentCondition(conditions, rule.Type, generation); cond != nil && cond.Status == rule.TriggerStatus {
			return InternalObjectState{
				Observed: true,
				Stalled:  gaveUp,
				Status:   metav1.ConditionFalse,
				Reason:   rule.Reason,
				Message:  cond.Message,
			}
		}
	}

	ready, observed := conditionObserved(conditions, helmv1alpha1.ConditionTypeReady, generation)
	if !observed {
		if gaveUp {
			// No rule matched and no verdict was reached, so the object's own account is all there is.
			return ownState(metav1.ConditionFalse, stalled, true)
		}

		return reconciling
	}

	return ownState(ready.Status, ready, gaveUp)
}

func ownState(status metav1.ConditionStatus, cond *metav1.Condition, stalled bool) InternalObjectState {
	state := InternalObjectState{
		Observed: true,
		Stalled:  stalled,
		Status:   status,
		Reason:   cond.Reason,
		Message:  cond.Message,
	}

	switch reason, known := upstreamFailureReasons[cond.Reason]; {
	case status == metav1.ConditionTrue:
	case status == metav1.ConditionUnknown:
		state.Reason = helmv1alpha1.ReasonReconciling
	case known:
		state.Reason = reason
	default:
		state.Reason = helmv1alpha1.ReasonFailed
		state.Message = cond.Reason
		if cond.Message != "" {
			state.Message += ": " + cond.Message
		}
	}

	return state
}

func matchTerminalMessage(conditions []metav1.Condition, generation int64) (InternalObjectState, bool) {
	for _, rule := range terminalMessageRules {
		for _, conditionType := range rule.ConditionTypes {
			cond := currentCondition(conditions, conditionType, generation)
			if cond == nil || !rule.Pattern.MatchString(cond.Message) {
				continue
			}

			return InternalObjectState{
				Observed: true,
				Stalled:  true,
				Status:   metav1.ConditionFalse,
				Reason:   rule.Reason,
				Message:  cond.Message,
			}, true
		}
	}

	return InternalObjectState{}, false
}

func conditionObserved(conditions []metav1.Condition, conditionType string, generation int64) (*metav1.Condition, bool) {
	// Stricter than currentCondition: Ready is the one condition read as proof the spec was observed.
	cond := apimeta.FindStatusCondition(conditions, conditionType)
	if cond == nil || cond.ObservedGeneration != generation {
		return cond, false
	}

	return cond, true
}

func currentCondition(conditions []metav1.Condition, conditionType string, generation int64) *metav1.Condition {
	// A condition with no observedGeneration counts as current: both controllers stamp what they write.
	cond := apimeta.FindStatusCondition(conditions, conditionType)
	if cond == nil || (cond.ObservedGeneration != 0 && cond.ObservedGeneration != generation) {
		return nil
	}

	return cond
}
