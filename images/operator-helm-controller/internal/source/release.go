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

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/deckhouse/operator-helm/internal/manager/status"
)

// RepositoryRef names the repository a release takes its chart from. Kind is one
// of the repository kinds of the API; Namespace is empty for a cluster-scoped kind.
// The adapter resolves the release's spec into this one shape — a single field for
// the addon, a pair of mutually exclusive fields for the application — so no
// service ever sees the difference.
type RepositoryRef struct {
	Kind      string
	Namespace string
	Name      string
}

// ChartRef identifies one chart version of one repository: what a release asks
// for, and what its status records as last applied.
type ChartRef struct {
	Repository RepositoryRef
	Chart      string
	Version    string
}

// ReleaseNames are the names of the internal objects derived from one release.
// ServiceAccount is empty for a family that does not impersonate.
type ReleaseNames struct {
	HelmChart      string
	HelmRelease    string
	OCIRepository  string
	ServiceAccount string
}

// Release is a release resource of any kind as the services see it. Object returns
// the API object itself — the only thing ever handed to the client; the adapter is
// not registered in the scheme. Values and the status setters point into that
// object, so a write through them lands on it.
type Release interface {
	Object() status.ObjectWithConditions
	// Kind is the API kind of the object, for messages and log lines.
	Kind() string
	Name() string
	Namespace() string
	Generation() int64

	ChartRef() ChartRef
	// TargetNamespace is where the release is deployed.
	TargetNamespace() string
	Values() *apiextensionsv1.JSON
	MaintenanceActivated() bool
	MaintenanceEnabled() bool
	ForceReconcileRequired() bool
	// ReleaseName is the Helm release name, at most 53 characters.
	ReleaseName() string

	// SourceLabels are carried by every internal object derived from this release,
	// including managed-by; the watch mappers read them back.
	SourceLabels() map[string]string
	// HelmChartLabels are SourceLabels plus the label naming the catalog object the
	// chart is taken from.
	HelmChartLabels() map[string]string
	InternalNames() ReleaseNames

	// LastAppliedChart is nil until a first successful deployment.
	LastAppliedChart() *ChartRef
	// SetLastAppliedChart replaces the record wholesale: a merge would leave a stale
	// repository field next to a new one on a kind with two reference fields.
	SetLastAppliedChart(ChartRef)
	LastAppliedValues() *apiextensionsv1.JSON
	SetLastAppliedValues(*apiextensionsv1.JSON)
	SetLastForceReconcileTime(metav1.Time)
	// IsChartStatusInfoOutdated reports whether the desired chart differs from the
	// last applied one.
	IsChartStatusInfoOutdated() bool
}

// RepositoryResolver loads the repository a release references and the catalog of
// that repository's kind. The application family picks between two repository
// kinds; the addon family has one.
type RepositoryResolver interface {
	Resolve(ctx context.Context, ref RepositoryRef) (Repository, Catalog, error)
}

// ChartClaim guards the uniqueness of a repository/chart pair across the releases
// of one family. The addon family enforces it with a Lease; the application family
// does not enforce it at all.
type ChartClaim interface {
	// Acquire reports whether the release holds the claim on its pair; when it does
	// not, holder names the release that does.
	Acquire(ctx context.Context, rel Release) (acquired bool, holder string, err error)
	// ReleaseStale frees claims this release still holds on pairs it no longer
	// references.
	ReleaseStale(ctx context.Context, rel Release) error
	// Release frees the claim on the release's current pair.
	Release(ctx context.Context, rel Release) error
}

// NoChartClaim is the ChartClaim of a family without a uniqueness rule: every
// release holds its own pair, and the conflict branch of the reconciler is never
// taken.
type NoChartClaim struct{}

func (NoChartClaim) Acquire(_ context.Context, rel Release) (bool, string, error) {
	return true, rel.Name(), nil
}

func (NoChartClaim) ReleaseStale(context.Context, Release) error { return nil }

func (NoChartClaim) Release(context.Context, Release) error { return nil }

// TargetNamespaceEnsurer makes sure the namespace a release deploys into exists.
type TargetNamespaceEnsurer interface {
	EnsureTargetNamespace(ctx context.Context, rel Release) error
}

// ExistingTargetNamespace is the ensurer of a family whose target namespace is the
// release's own: it exists by definition, or the release could not.
type ExistingTargetNamespace struct{}

func (ExistingTargetNamespace) EnsureTargetNamespace(context.Context, Release) error {
	return nil
}

// AccessManager provides the identity a release is applied with. The application
// family creates a ServiceAccount, a Role and a RoleBinding and names the account
// on the HelmRelease; the addon family applies charts as helm-controller itself.
type AccessManager interface {
	EnsureAccess(ctx context.Context, rel Release) error
	CleanupAccess(ctx context.Context, rel Release) error
}

// NoAccess is the AccessManager of a family that does not impersonate.
type NoAccess struct{}

func (NoAccess) EnsureAccess(context.Context, Release) error { return nil }

func (NoAccess) CleanupAccess(context.Context, Release) error { return nil }

// ReleaseLister lists the releases of one family that consume a repository, or
// only those consuming one chart of it when chartName is not empty. It is how a
// repository finds its consumers without knowing their kind.
type ReleaseLister func(ctx context.Context, repo Repository, chartName string) ([]Release, error)
