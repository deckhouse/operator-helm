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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type ObjectWithConditions interface {
	client.Object
	GetConditions() *[]metav1.Condition
	GetGeneration() int64
	GetObservedGeneration() int64
	SetObservedGeneration(int64)
	GetConditionTypesForUpdate() []string
}

type Manager struct {
	client.Client
}

func NewManager(c client.Client) *Manager {
	return &Manager{
		Client: c,
	}
}

// PatchStatus applies mutate to the object and patches the status subresource
// when it actually changed. It is the thin apply path used by reconcilers that
// compute the whole desired status themselves.
//
// The whole object is compared rather than its status alone: mutate only ever
// reaches the status, so the two are the same comparison, and this one needs no
// method on the object to reach a field the interface would otherwise have to
// hand out as an any.
func (s *Manager) PatchStatus(ctx context.Context, obj ObjectWithConditions, mutate func()) error {
	oldObj := obj.DeepCopyObject().(ObjectWithConditions)

	mutate()

	if reflect.DeepEqual(obj, oldObj) {
		return nil
	}

	if err := s.Status().Patch(ctx, obj, client.MergeFrom(oldObj)); err != nil {
		return fmt.Errorf("patching status: %w", err)
	}

	return nil
}
