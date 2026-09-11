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

package source

import (
	"context"

	"k8s.io/apimachinery/pkg/runtime/schema"

	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
	"github.com/deckhouse/operator-helm/internal/manager/status"
)

// InternalNames are the names of the internal objects derived from one
// repository. They are computed by the adapter: the scheme differs between
// the addon family (frozen, hash only on truncation) and the application family
// (namespace-aware, hash always present), and no service should know which is
// which.
type InternalNames struct {
	HelmRepository string
	AuthSecret     string
	TLSSecret      string
}

// Repository is a repository resource of any kind as the services see it. Object
// returns the API object itself, which is what the client reads, updates and
// patches: the adapter is not registered in the scheme and must never be handed to
// the client directly.
type Repository interface {
	Object() status.ObjectWithConditions
	Name() string
	Namespace() string
	Generation() int64
	// OwnerGVK is the GroupVersionKind stamped into the owner reference of the
	// catalog objects this repository produces.
	OwnerGVK() schema.GroupVersionKind

	URL() string
	Auth() *helmv1alpha1.RepositoryAuth
	CACertificate() string
	InsecureSkipVerify() bool

	// Status points into the object, so a write through it lands on the object.
	Status() *helmv1alpha1.RepositoryStatus
	ForceReconcileRequired() bool

	// SourceLabels are the labels every internal object derived from this
	// repository carries, including managed-by; the watch mappers read them back.
	SourceLabels() map[string]string
	InternalNames() InternalNames
}

// ConsumerForcer pushes a force reconcile request onto the internal sources of
// the resources that consume a repository. Which resources those are depends on the
// family, so the reconciler is handed an implementation instead of looking them up.
type ConsumerForcer interface {
	ForceReconcileConsumers(ctx context.Context, repo Repository) error
}

// NoConsumers is the ConsumerForcer of a family whose consumers do not exist yet.
// The application family uses it until HelmApplication is reconciled by the
// controller; that implementation replaces it.
type NoConsumers struct{}

func (NoConsumers) ForceReconcileConsumers(context.Context, Repository) error {
	return nil
}
