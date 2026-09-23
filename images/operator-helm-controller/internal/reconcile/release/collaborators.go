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
	"github.com/deckhouse/operator-helm/internal/services"
	"github.com/deckhouse/operator-helm/internal/source"
)

// The collaborators of one reconcile pass, declared where they are used rather than
// where they are implemented: a consumer names the behaviour it needs, and anything
// that offers it fits. That is what lets a family opt out of a step with a null
// implementation, and a test replace one step without standing up the rest.

// RepositoryResolver loads the repository a release references and the catalog of
// that repository's kind. The application family picks between two repository
// kinds; the addon family has one.
type RepositoryResolver interface {
	Resolve(ctx context.Context, ref source.RepositoryRef) (source.Repository, source.Catalog, error)
}

// ChartClaim guards the uniqueness of a repository/chart pair across the releases
// of one family. The addon family enforces it with a Lease; the application family
// does not enforce it at all.
type ChartClaim interface {
	// Acquire reports whether the release holds the claim on its pair; when it does
	// not, holder names the release that does.
	Acquire(ctx context.Context, rel source.Release) (acquired bool, holder string, err error)
	// ReleaseStale frees claims this release still holds on pairs it no longer
	// references.
	ReleaseStale(ctx context.Context, rel source.Release) error
	// Release frees the claim on the release's current pair.
	Release(ctx context.Context, rel source.Release) error
}

// TargetNamespaceEnsurer makes sure the namespace a release deploys into exists.
type TargetNamespaceEnsurer interface {
	EnsureTargetNamespace(ctx context.Context, rel source.Release) error
}

// RBACManager provides the identity a release is applied with. The application
// family creates a ServiceAccount, a Role and a RoleBinding and names the account
// on the HelmRelease; the addon family applies charts as helm-controller itself.
type RBACManager interface {
	EnsureRBAC(ctx context.Context, rel source.Release) services.RBACOutcome
	CleanupRBAC(ctx context.Context, rel source.Release) error
}

// ChartManager owns the internal HelmChart of a release, the source object of a
// repository that hands out packaged archives.
type ChartManager interface {
	EnsureHelmChart(ctx context.Context, rel source.Release, repo source.Repository) services.ChartOutcome
	// CleanupHelmChart returns the object while it is still present, so the caller
	// can wait for it to actually go away.
	CleanupHelmChart(ctx context.Context, names source.ReleaseNames) (*sourcev1.HelmChart, error)
}

// OCIRepoManager owns the internal OCIRepository of a release, the source object of
// a repository that hands out registry artifacts.
type OCIRepoManager interface {
	EnsureInternalOCIRepository(
		ctx context.Context,
		rel source.Release,
		repo source.Repository,
		src chartsource.Source,
		version *helmv1alpha1.ChartVersion,
	) services.OCIRepoOutcome
	RemoveOCIRepository(ctx context.Context, names source.ReleaseNames) (*sourcev1.OCIRepository, error)
}

// ReleaseManager owns the internal HelmRelease: the object helm-controller acts on.
type ReleaseManager interface {
	EnsureHelmRelease(
		ctx context.Context,
		rel source.Release,
		sourceKind chartsource.Kind,
		artifactRevision string,
	) services.ReleaseOutcome
	CleanupHelmRelease(ctx context.Context, names source.ReleaseNames) (*helmv2.HelmRelease, error)
	// SyncReleaseSpec keeps a release that is being deleted in step with its spec, so
	// an uninstall blocked by a bad parameter can be unblocked by correcting it.
	SyncReleaseSpec(ctx context.Context, rel source.Release, existing *helmv2.HelmRelease) error
}

// MaintenanceManager suspends and resumes the internal HelmRelease.
type MaintenanceManager interface {
	IsMaintenanceModeChangeRequired(rel source.Release) bool
	EnsureMaintenanceMode(ctx context.Context, rel source.Release) services.MaintenanceOutcome
}

// NoChartClaim is the ChartClaim of a family without a uniqueness rule: every
// release holds its own pair, and the conflict branch of the reconciler is never
// taken.
type NoChartClaim struct{}

func (NoChartClaim) Acquire(_ context.Context, rel source.Release) (bool, string, error) {
	return true, rel.Name(), nil
}

func (NoChartClaim) ReleaseStale(context.Context, source.Release) error { return nil }

func (NoChartClaim) Release(context.Context, source.Release) error { return nil }

// ExistingTargetNamespace is the ensurer of a family whose target namespace is the
// release's own: it exists by definition, or the release could not.
type ExistingTargetNamespace struct{}

func (ExistingTargetNamespace) EnsureTargetNamespace(context.Context, source.Release) error {
	return nil
}

// NoRBAC is the RBACManager of a family that does not impersonate.
type NoRBAC struct{}

func (NoRBAC) EnsureRBAC(context.Context, source.Release) services.RBACOutcome {
	return services.RBACOutcome{}
}

func (NoRBAC) CleanupRBAC(context.Context, source.Release) error { return nil }
