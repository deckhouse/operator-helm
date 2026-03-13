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
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/deckhouse/operator-helm/internal/common"
)

type BaseService struct {
	Client client.Client
	Scheme *runtime.Scheme
}

func (s *BaseService) ensureResourceDeleted(ctx context.Context, nn types.NamespacedName, obj client.Object) error {
	err := s.Client.Get(ctx, nn, obj)
	if err != nil {
		return client.IgnoreNotFound(err)
	}

	if err := s.Client.Delete(ctx, obj); err != nil {
		return fmt.Errorf("failed to delete resource %s/%s: %w", nn.Namespace, nn.Name, err)
	}

	return nil
}

type CommonState struct {
	ReconcileError error
}

type ResourceStatus struct {
	Status             metav1.ConditionStatus
	ObservedGeneration int64
	Reason             string
	Message            string
	Err                error
}

func (s ResourceStatus) IsReady() bool {
	return s.Status == metav1.ConditionTrue
}

type statusProxy struct {
	StatusProvider
	newType string
}

func (p statusProxy) GetConditionType() string { return p.newType }

func AsCondition(res StatusProvider, conditionType string) StatusProvider {
	return statusProxy{StatusProvider: res, newType: conditionType}
}

func Success(obj client.Object) ResourceStatus {
	return ResourceStatus{
		Status:             metav1.ConditionTrue,
		Reason:             common.ReasonSuccess,
		ObservedGeneration: obj.GetGeneration(),
	}
}

func Failed(obj client.Object, reason, message string, err error) ResourceStatus {
	return ResourceStatus{
		Status:             metav1.ConditionFalse,
		Reason:             reason,
		ObservedGeneration: obj.GetGeneration(),
		Message:            message,
		Err:                err,
	}
}

func Unknown(obj client.Object, reason string) ResourceStatus {
	return ResourceStatus{
		Status:             metav1.ConditionUnknown,
		Reason:             reason,
		ObservedGeneration: obj.GetGeneration(),
	}
}
