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

package helmclusteraddonrepository

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TODO: need to re-work these statuses according to adr

func MapInternalStatusToClusterConditions(internalConditions []metav1.Condition) []metav1.Condition {
	now := metav1.Now()

	var readyCond *metav1.Condition

	for i := range internalConditions {
		if internalConditions[i].Type == ConditionTypeReady {
			readyCond = &internalConditions[i]

			break
		}
	}

	if readyCond == nil {
		return []metav1.Condition{
			{
				Type:               ConditionTypeReady,
				Status:             metav1.ConditionUnknown,
				Reason:             ReasonInternalNotReady,
				Message:            "Processing",
				LastTransitionTime: now,
			},
		}
	}

	reason := ReasonInternalNotReady
	if readyCond.Status == metav1.ConditionTrue {
		reason = ReasonInternalReady
	}

	return []metav1.Condition{
		{
			Type:               ConditionTypeReady,
			Status:             readyCond.Status,
			Reason:             reason,
			Message:            readyCond.Message,
			LastTransitionTime: now,
		},
	}
}
