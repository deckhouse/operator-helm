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
	"context"
	"fmt"
	"reflect"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

type ObjectWithConditions interface {
	client.Object
	GetConditions() *[]metav1.Condition
	GetGeneration() int64
	GetObservedGeneration() int64
	SetObservedGeneration(int64)
	GetConditionTypesForUpdate() []string
	GetStatus() interface{}
}

type Provider interface {
	GetStatus() Status
	GetConditionType() string
}

type GenerationProvider interface {
	GetObservedGeneration() int64
}

type Manager struct {
	client.Client
}

func NewManager(c client.Client) *Manager {
	return &Manager{
		Client: c,
	}
}

type MutatorFunc func(ObjectWithConditions, []Provider) (ObjectWithConditions, []Provider)

var NoopStatusMutator = MutatorFunc(func(o ObjectWithConditions, s []Provider) (ObjectWithConditions, []Provider) { return o, s })

type MapperFunc func(string, Status) Status

var NoopStatusMapper = MapperFunc(func(_ string, status Status) Status {
	return status
})

func (s *Manager) Update(ctx context.Context, obj ObjectWithConditions, mutatorFunc MutatorFunc, statusMapperFunc MapperFunc, results ...Provider) error {
	logger := log.FromContext(ctx)

	oldObj := obj.DeepCopyObject().(ObjectWithConditions)

	if mutatorFunc != nil {
		obj, results = mutatorFunc(obj, results)
	}

	conditions := obj.GetConditions()
	currentGen := obj.GetGeneration()
	minObservedGen := currentGen

	for _, res := range results {
		if res == nil {
			continue
		}

		status := res.GetStatus()

		status = statusMapperFunc(res.GetConditionType(), status)

		if status.Status == "" || status.Reason == "" {
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

func (s *Manager) InitializeConditions(ctx context.Context, obj ObjectWithConditions, conditionTypes ...string) error {
	oldObj := obj.DeepCopyObject().(ObjectWithConditions)
	patchBase := client.MergeFrom(oldObj)

	conditions := obj.GetConditions()
	changed := false

	for _, t := range conditionTypes {
		if meta.FindStatusCondition(*conditions, t) == nil {
			meta.SetStatusCondition(conditions, metav1.Condition{
				Type:   t,
				Status: metav1.ConditionUnknown,
				Reason: "Initialized",
			})
			changed = true
		}
	}

	if changed {
		logger := log.FromContext(ctx)
		logger.Info("Initializing conditions", "name", obj.GetName(), "types", conditionTypes)

		if err := s.Client.Status().Patch(ctx, obj, patchBase); err != nil {
			return fmt.Errorf("initializing conditions: %w", err)
		}
	}

	return nil
}

func DetermineConditions(obj ObjectWithConditions, results ...Provider) []Provider {
	var result []Provider

	conditionTypes := obj.GetConditionTypesForUpdate()
	if len(results) == 0 {
		return result
	}

	var decisionRes Provider
	for _, res := range results {
		if res == nil {
			continue
		}

		status := res.GetStatus()
		if status.Status == "" || status.Reason == "" {
			continue
		}

		if status.NotReflectable {
			result = append(result, res)
			continue
		}

		decisionRes = res
		if !status.IsReady() {
			break
		}
	}

	if decisionRes == nil {
		return result
	}

	for _, conditionType := range conditionTypes {
		result = append(result, AsCondition(decisionRes, conditionType))
	}

	return result
}

type Status struct {
	ConditionType      string
	Observed           bool
	Status             metav1.ConditionStatus
	ObservedGeneration int64
	Reason             string
	Message            string
	// NotReflectable marks a result that is appended as its own condition directly,
	// bypassing the "decision result" logic in DetermineConditions. When true, the
	// result does not participate in selecting the single decision result that gets
	// projected across all condition types returned by GetConditionTypesForUpdate.
	NotReflectable bool
	Err            error
}

func (s Status) IsReady() bool {
	return s.Status == metav1.ConditionTrue && s.Observed
}
