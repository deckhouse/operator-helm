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

package repository

import (
	"context"

	sourcev1 "github.com/fluxcd/source-controller/api/v1"

	"github.com/deckhouse/operator-helm/internal/chartsource"
	"github.com/deckhouse/operator-helm/internal/contract"
	"github.com/deckhouse/operator-helm/internal/services"
)

// The collaborators of one reconcile pass, declared where they are used rather than
// where they are implemented, as the release package declares its own.

// Secrets owns the auxiliary secrets a repository needs to be read: the
// credentials of its registry and, for an oci:// one, the TLS material.
type Secrets interface {
	Ensure(ctx context.Context, repo contract.Repository, repoType chartsource.Kind) error
	Cleanup(ctx context.Context, names contract.InternalNames) error
}

// HelmRepositories owns the internal HelmRepository, which exists only for
// a repository that hands out packaged archives.
type HelmRepositories interface {
	Ensure(ctx context.Context, repo contract.Repository) (services.InternalRepositoryState, error)
	// Remove drops the object a repository no longer needs, after its
	// url moved from helm to oci.
	Remove(ctx context.Context, names contract.InternalNames) error
	// Cleanup returns the object while it is still present, so the
	// caller can wait for it to actually go away.
	Cleanup(ctx context.Context, names contract.InternalNames) (*sourcev1.HelmRepository, error)
}

// Catalogs reads the remote and writes the chart catalog of one
// repository.
type Catalogs interface {
	Sync(ctx context.Context, repo contract.Repository, repoType chartsource.Kind) services.SyncOutcome
	// MigrateNames moves the catalog objects to their current names.
	//
	// TRANSITIONAL: remove together with the catalog's own migration.
	MigrateNames(ctx context.Context, repo contract.Repository) error
}
