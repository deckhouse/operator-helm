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

package release

import (
	"context"

	helmv2 "github.com/fluxcd/helm-controller/api/v2"
	sourcev1 "github.com/fluxcd/source-controller/api/v1"

	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
	"github.com/deckhouse/operator-helm/internal/chartsource"
	"github.com/deckhouse/operator-helm/internal/contract"
	"github.com/deckhouse/operator-helm/internal/internalobj"
	"github.com/deckhouse/operator-helm/internal/rbac"
	"github.com/deckhouse/operator-helm/internal/status"
)

// The interfaces one reconcile pass depends on, declared where they are used.

// Repositories loads the repository a release references and the catalog of that repository's
// kind. The application family picks between two repository kinds; the addon family has one.
type Repositories interface {
	Resolve(ctx context.Context, ref contract.RepositoryRef) (contract.Repository, contract.Catalog, error)
}

// Claims guards the uniqueness of a repository/chart pair across the releases of one family.
// The addon family enforces it with a Lease; the application family does not enforce it.
type Claims interface {
	// Acquire reports whether the release holds the claim on its pair; when it does
	// not, holder names the release that does.
	Acquire(ctx context.Context, rel contract.Release) (acquired bool, holder string, err error)
	// DropStale frees claims this release still holds on pairs it no longer references and runs.
	DropStale(ctx context.Context, rel contract.Release) error
	// Drop frees the claim on the release's current pair.
	Drop(ctx context.Context, rel contract.Release) error
	// DropAll frees every claim this release holds. It is for teardown.
	DropAll(ctx context.Context, rel contract.Release) error
}

// Namespaces makes sure the namespace a release deploys into exists, and answers whether it
// is on its way out, which the teardown path needs.
type Namespaces interface {
	EnsureTarget(ctx context.Context, rel contract.Release) error
	Terminating(ctx context.Context, rel contract.Release) (bool, error)
}

// Identity provides the identity a release is applied with. The application family creates a
// ServiceAccount, a Role and a RoleBinding; the addon family applies charts as the controller.
type Identity interface {
	Ensure(ctx context.Context, rel contract.Release) rbac.IdentityOutcome
	// Cleanup returns the identity object still present, for the caller to wait on.
	Cleanup(ctx context.Context, rel contract.Release) (status.DeletingResource, error)
	// RemoveAccount deletes the account alone, ahead of the ordinary order, and waits for nothing.
	RemoveAccount(ctx context.Context, rel contract.Release) error
}

// Charts owns the internal HelmChart of a release, the source object of a repository that
// hands out packaged archives.
type Charts interface {
	Ensure(ctx context.Context, rel contract.Release, repo contract.Repository) internalobj.ChartOutcome
	// Cleanup returns the object while it is still present, for the caller to wait on.
	Cleanup(ctx context.Context, names contract.ReleaseNames) (*sourcev1.HelmChart, error)
}

// OCIRepositories owns the internal OCIRepository of a release, the source object of
// a repository that hands out registry artifacts.
type OCIRepositories interface {
	Ensure(
		ctx context.Context,
		rel contract.Release,
		repo contract.Repository,
		src chartsource.Source,
		version *helmv1alpha1.ChartVersion,
	) internalobj.OCIRepoOutcome
	Remove(ctx context.Context, names contract.ReleaseNames) (*sourcev1.OCIRepository, error)
}

// Releases owns the internal HelmRelease: the object helm-controller acts on.
type Releases interface {
	Ensure(
		ctx context.Context,
		rel contract.Release,
		sourceKind chartsource.Kind,
		artifactRevision string,
		// artifactCurrent reports whether that revision is the one the current spec asks for.
		artifactCurrent bool,
	) internalobj.ReleaseOutcome
	Cleanup(ctx context.Context, names contract.ReleaseNames) (*helmv2.HelmRelease, error)
	// SyncSpec keeps a release that is being deleted in step with its spec, so an uninstall
	// blocked by a bad parameter can be unblocked by correcting it.
	SyncSpec(ctx context.Context, rel contract.Release, existing *helmv2.HelmRelease) error
}

// Maintenance suspends and resumes the internal HelmRelease.
type Maintenance interface {
	ChangeRequired(rel contract.Release) bool
	Ensure(ctx context.Context, rel contract.Release) internalobj.MaintenanceOutcome
}

// NoClaims is the Claims of a family without a uniqueness rule: every release holds its own
// pair, and the conflict branch of the reconciler is never taken.
type NoClaims struct{}

func (NoClaims) Acquire(_ context.Context, rel contract.Release) (bool, string, error) {
	return true, rel.Name(), nil
}

func (NoClaims) DropStale(context.Context, contract.Release) error { return nil }

func (NoClaims) Drop(context.Context, contract.Release) error { return nil }

func (NoClaims) DropAll(context.Context, contract.Release) error { return nil }

// NoIdentity is the Identity of a family that does not impersonate.
type NoIdentity struct{}

func (NoIdentity) Ensure(context.Context, contract.Release) rbac.IdentityOutcome {
	return rbac.IdentityOutcome{}
}

func (NoIdentity) Cleanup(context.Context, contract.Release) (status.DeletingResource, error) {
	return nil, nil
}

func (NoIdentity) RemoveAccount(context.Context, contract.Release) error { return nil }
