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
// check — and refuses to delete an application in maintenance mode, unless that
// namespace is itself already terminating.
package helmapplication

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
	"github.com/deckhouse/operator-helm/internal/utils"
)

func SetupWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr, &helmv1alpha1.HelmApplication{}).
		WithValidator(&HelmApplicationWebhookValidator{Client: mgr.GetClient()}).
		Complete()
}

var _ admission.Validator[*helmv1alpha1.HelmApplication] = (*HelmApplicationWebhookValidator)(nil)

type HelmApplicationWebhookValidator struct {
	Client client.Client
}

func (v *HelmApplicationWebhookValidator) ValidateCreate(_ context.Context, app *helmv1alpha1.HelmApplication) (admission.Warnings, error) {
	return nil, validateNotSystemNamespace(app)
}

func (v *HelmApplicationWebhookValidator) ValidateUpdate(_ context.Context, _, newObj *helmv1alpha1.HelmApplication) (admission.Warnings, error) {
	return nil, validateNotSystemNamespace(newObj)
}

func (v *HelmApplicationWebhookValidator) ValidateDelete(ctx context.Context, app *helmv1alpha1.HelmApplication) (admission.Warnings, error) {
	if !app.MaintenanceModeActivated() {
		return nil, nil
	}

	// Namespace deletion deletes the namespace's objects one by one and waits for
	// each of them; a DELETE webhook that denies would make that wait never end,
	// leaving the namespace Terminating forever with no way out other than editing
	// the application. Maintenance mode protects an application from being deleted
	// by mistake, not from its namespace going away, so the refusal is dropped once
	// the namespace is on its way out.
	if v.namespaceTerminating(ctx, app.Namespace) {
		return nil, nil
	}

	return nil, fmt.Errorf("helmapplication/%s cannot be deleted while maintenance mode is active", app.Name)
}

// namespaceTerminating reports whether the application's own namespace is already
// gone or being deleted. A read error other than NotFound is deliberately treated
// as "not terminating": the refusal is the safe answer, and a transient API error
// must not turn into a way past it.
func (v *HelmApplicationWebhookValidator) namespaceTerminating(ctx context.Context, name string) bool {
	namespace := &corev1.Namespace{}
	if err := v.Client.Get(ctx, client.ObjectKey{Name: name}, namespace); err != nil {
		return apierrors.IsNotFound(err)
	}

	return !namespace.DeletionTimestamp.IsZero()
}

func validateNotSystemNamespace(app *helmv1alpha1.HelmApplication) error {
	if utils.IsSystemNamespace(app.Namespace) {
		return fmt.Errorf("helmapplication/%s cannot be created in system namespace %s", app.Name, app.Namespace)
	}

	return nil
}
