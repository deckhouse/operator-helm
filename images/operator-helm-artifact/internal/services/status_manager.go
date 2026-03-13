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

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	ReasonReconciling = "Reconciling"
)

type ObjectWithConditions interface {
	client.Object
	GetConditions() *[]metav1.Condition
	GetGeneration() int64
	GetObservedGeneration() int64
	SetObservedGeneration(int64)
	GetStatus() interface{}
}

type StatusProvider interface {
	GetStatus() ResourceStatus
	GetConditionType() string
}

type GenerationProvider interface {
	GetObservedGeneration() int64
}

type StatusManager struct {
	client.Client

	FieldOwner string
}

func NewStatusManager(c client.Client, fieldOwner string) *StatusManager {
	return &StatusManager{
		Client:     c,
		FieldOwner: fieldOwner,
	}
}

func (s *StatusManager) Update(ctx context.Context, obj ObjectWithConditions, results ...StatusProvider) error {
	logger := log.FromContext(ctx)

	oldObj := obj.DeepCopyObject().(ObjectWithConditions)
	conditions := obj.GetConditions()
	currentGen := obj.GetGeneration()
	minObservedGen := currentGen

	for _, res := range results {
		if res == nil {
			continue
		}

		status := res.GetStatus()
		if status.Status == "" {
			continue
		}

		if status.Err != nil {
			logger.Error(status.Err, status.Message,
				"condition", res.GetConditionType(),
				"reason", status.Reason)
		}

		meta.SetStatusCondition(conditions, metav1.Condition{
			Type:               res.GetConditionType(),
			Status:             status.Status,
			Reason:             status.Reason,
			Message:            status.Message,
			ObservedGeneration: status.ObservedGeneration,
		})

		if status.ObservedGeneration < minObservedGen {
			minObservedGen = status.ObservedGeneration
		}
	}

	oldObservedGen := oldObj.GetObservedGeneration()
	if minObservedGen > oldObservedGen {
		obj.SetObservedGeneration(minObservedGen)
	} else {
		obj.SetObservedGeneration(oldObservedGen)
	}

	if reflect.DeepEqual(obj.GetStatus(), oldObj.GetStatus()) {
		return nil
	}

	return s.Status().Patch(ctx, obj, client.MergeFrom(oldObj))
}

func (s *StatusManager) InitializeConditions(ctx context.Context, obj ObjectWithConditions, conditionTypes ...string) error {
	oldObj := obj.DeepCopyObject().(ObjectWithConditions)
	patchBase := client.MergeFrom(oldObj)

	conditions := obj.GetConditions()
	changed := false

	for _, t := range conditionTypes {
		if meta.FindStatusCondition(*conditions, t) == nil {
			meta.SetStatusCondition(conditions, metav1.Condition{
				Type:    t,
				Status:  metav1.ConditionUnknown,
				Reason:  "Initialization",
				Message: "Condition initialized, waiting for reconciliation",
			})
			changed = true
		}
	}

	if changed {
		logger := log.FromContext(ctx)
		logger.Info("Initializing conditions", "name", obj.GetName(), "types", conditionTypes)
		return s.Client.Status().Patch(ctx, obj, patchBase)
	}

	return nil
}
