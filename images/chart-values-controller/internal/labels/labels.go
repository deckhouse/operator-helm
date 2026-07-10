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

package labels

const (
	// ManagedBy marks auxiliary source resources created by this controller so
	// they can be distinguished from resources managed by operator-helm-controller.
	ManagedBy = "app.kubernetes.io/managed-by"

	// ManagedByValue is the value of the ManagedBy label.
	ManagedByValue = "chart-values-controller"

	// ExpiresAtAnnotation holds the RFC3339 UTC timestamp after which an auxiliary
	// resource may be deleted by the cleanup reconciler.
	ExpiresAtAnnotation = "chart-values.deckhouse.io/expires-at"
)
