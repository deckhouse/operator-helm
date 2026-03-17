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

type statusProxy struct {
	Provider
	newType string
}

func (p statusProxy) GetConditionType() string { return p.newType }

func AsCondition(res Provider, conditionType string) Provider {
	return statusProxy{Provider: res, newType: conditionType}
}

func Success(obj client.Object) Status {
	return Status{
		Observed:           true,
		Status:             metav1.ConditionTrue,
		Reason:             helmv1alpha1.ReasonSuccess,
		ObservedGeneration: obj.GetGeneration(),
	}
}

func Failed(obj client.Object, reason, message string, err error) Status {
	return Status{
		Observed:           true,
		Status:             metav1.ConditionFalse,
		Reason:             reason,
		ObservedGeneration: obj.GetGeneration(),
		Message:            message,
		Err:                err,
	}
}

func Unknown(obj client.Object, reason string) Status {
	return Status{
		Status:             metav1.ConditionUnknown,
		Reason:             reason,
		ObservedGeneration: obj.GetGeneration(),
	}
}

func Empty() Status {
	return Status{}
}

func IsConditionObserved(conditions []metav1.Condition, conditionType string, generation int64) (*metav1.Condition, bool) {
	cond := meta.FindStatusCondition(conditions, conditionType)
	if cond == nil || cond.ObservedGeneration != generation {
		return cond, false
	}

	return cond, true
}
