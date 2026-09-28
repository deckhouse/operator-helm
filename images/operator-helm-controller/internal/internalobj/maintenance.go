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
	"context"
	"fmt"

	helmv2 "github.com/fluxcd/helm-controller/api/v2"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/deckhouse/operator-helm/internal/contract"
)

type Maintenance struct {
	client          client.Client
	targetNamespace string
}

func NewMaintenance(client client.Client, targetNamespace string) *Maintenance {
	return &Maintenance{client: client, targetNamespace: targetNamespace}
}

func (s *Maintenance) Ensure(ctx context.Context, rel contract.Release) MaintenanceOutcome {
	logger := log.FromContext(ctx)

	activated := rel.MaintenanceActivated()
	if activated {
		logger.Info("Enabling maintenance mode")
	} else {
		logger.Info("Disabling maintenance mode")
	}

	if err := s.updateHelmReleaseSuspendState(ctx, rel.InternalNames(), activated); err != nil {
		return MaintenanceOutcome{Err: err, Activated: activated}
	}

	return MaintenanceOutcome{Activated: activated}
}

// ChangeRequired reports whether the suspend flag on the internal HelmRelease still
// has to be moved: what the spec asks for against what the last pass wrote as Managed.
func (s *Maintenance) ChangeRequired(rel contract.Release) bool {
	return rel.MaintenanceActivated() != rel.MaintenanceEnabled()
}

func (s *Maintenance) updateHelmReleaseSuspendState(ctx context.Context, names contract.ReleaseNames, suspend bool) error {
	helmRelease := &helmv2.HelmRelease{}
	if err := s.client.Get(ctx, types.NamespacedName{
		Name:      names.HelmRelease,
		Namespace: s.targetNamespace,
	}, helmRelease); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("getting helm release: %w", err)
	}

	if helmRelease.Spec.Suspend == suspend {
		return nil
	}

	base := helmRelease.DeepCopy()
	helmRelease.Spec.Suspend = suspend

	if err := s.client.Patch(ctx, helmRelease, client.MergeFrom(base)); err != nil {
		return fmt.Errorf("setting helm release suspend state: %w", err)
	}

	return nil
}
