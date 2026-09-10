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

// Package helmapplication validates HelmApplication objects. Unlike the addon
// webhook it enforces no uniqueness: any number of applications may install the
// same chart. It rejects an application in a system namespace — the release
// deploys into the application's own namespace, so that is the namespace to
// check — and refuses to delete an application in maintenance mode.
package helmapplication

import (
	"context"
	"fmt"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
	"github.com/deckhouse/operator-helm/internal/utils"
)

func SetupWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr, &helmv1alpha1.HelmApplication{}).
		WithValidator(&HelmApplicationWebhookValidator{}).
		Complete()
}

var _ admission.Validator[*helmv1alpha1.HelmApplication] = (*HelmApplicationWebhookValidator)(nil)

type HelmApplicationWebhookValidator struct{}

func (v *HelmApplicationWebhookValidator) ValidateCreate(_ context.Context, app *helmv1alpha1.HelmApplication) (admission.Warnings, error) {
	return nil, validateNotSystemNamespace(app)
}

func (v *HelmApplicationWebhookValidator) ValidateUpdate(_ context.Context, _, newObj *helmv1alpha1.HelmApplication) (admission.Warnings, error) {
	return nil, validateNotSystemNamespace(newObj)
}

func (v *HelmApplicationWebhookValidator) ValidateDelete(_ context.Context, app *helmv1alpha1.HelmApplication) (admission.Warnings, error) {
	if app.MaintenanceModeActivated() {
		return nil, fmt.Errorf("helmapplication/%s cannot be deleted while maintenance mode is active", app.Name)
	}

	return nil, nil
}

func validateNotSystemNamespace(app *helmv1alpha1.HelmApplication) error {
	if utils.IsSystemNamespace(app.Namespace) {
		return fmt.Errorf("helmapplication/%s cannot be created in system namespace %s", app.Name, app.Namespace)
	}

	return nil
}
