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

package adapter

import (
	"context"
	"fmt"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/deckhouse/operator-helm/api/naming"
	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
	"github.com/deckhouse/operator-helm/internal/index"
	"github.com/deckhouse/operator-helm/internal/manager/status"
	"github.com/deckhouse/operator-helm/internal/source"
	"github.com/deckhouse/operator-helm/internal/utils"
)

// applicationPrefix prefixes every internal object derived from a HelmApplication:
// HelmChart, HelmRelease, OCIRepository and ServiceAccount share one name, as the
// addon's do. The same prefix starts the Helm release name.
const applicationPrefix = "hap"

var _ source.Release = (*ApplicationRelease)(nil)

// ApplicationRelease adapts a HelmApplication. This is the one place the two
// mutually exclusive repository fields are read: everything downstream sees the
// resolved kind, namespace and name. The release deploys into the application's
// own namespace and is applied as a ServiceAccount derived here.
type ApplicationRelease struct {
	obj *helmv1alpha1.HelmApplication
}

func NewApplicationRelease(obj *helmv1alpha1.HelmApplication) *ApplicationRelease {
	return &ApplicationRelease{obj: obj}
}

func EmptyApplicationRelease() source.Release {
	return NewApplicationRelease(&helmv1alpha1.HelmApplication{})
}

func (r *ApplicationRelease) Object() status.ObjectWithConditions { return r.obj }
func (r *ApplicationRelease) Kind() string                        { return helmv1alpha1.HelmApplicationKind }
func (r *ApplicationRelease) Name() string                        { return r.obj.Name }
func (r *ApplicationRelease) Namespace() string                   { return r.obj.Namespace }
func (r *ApplicationRelease) Generation() int64                   { return r.obj.Generation }

// repositoryRef resolves the XOR of the spec into one reference. A namespaced
// repository lives in the application's own namespace.
func (r *ApplicationRelease) repositoryRef(kind, name string) source.RepositoryRef {
	ref := source.RepositoryRef{Kind: kind, Name: name}
	if kind == helmv1alpha1.HelmApplicationRepositoryKind {
		ref.Namespace = r.obj.Namespace
	}

	return ref
}

func (r *ApplicationRelease) ChartRef() source.ChartRef {
	return source.ChartRef{
		Repository: r.repositoryRef(r.obj.RepositoryKind(), r.obj.RepositoryName()),
		Chart:      r.obj.Spec.Chart.Name,
		Version:    r.obj.Spec.Chart.Version,
	}
}

func (r *ApplicationRelease) TargetNamespace() string       { return r.obj.Namespace }
func (r *ApplicationRelease) Values() *apiextensionsv1.JSON { return r.obj.Spec.Values }
func (r *ApplicationRelease) MaintenanceActivated() bool    { return r.obj.MaintenanceModeActivated() }

func (r *ApplicationRelease) MaintenanceEnabled() bool { return r.obj.MaintenanceModeEnabled() }

func (r *ApplicationRelease) ForceReconcileRequired() bool { return r.obj.ForceReconcileRequired() }

func (r *ApplicationRelease) IsChartStatusInfoOutdated() bool {
	return r.obj.IsChartStatusInfoOutdated()
}

func (r *ApplicationRelease) LastAppliedValues() *apiextensionsv1.JSON {
	return r.obj.Status.LastAppliedValues
}

// ReleaseName is prefixed so an application cannot take over a release someone
// installed by hand under the same name in the same namespace; without the prefix
// helm-controller would upgrade that release instead of failing.
func (r *ApplicationRelease) ReleaseName() string {
	return utils.HelmReleaseName(applicationPrefix + "-" + r.obj.Name)
}

func (r *ApplicationRelease) SourceLabels() map[string]string {
	return map[string]string{
		helmv1alpha1.LabelManagedBy:                 helmv1alpha1.LabelManagedByValue,
		helmv1alpha1.HelmApplicationLabelSourceName: r.obj.Name,
		helmv1alpha1.LabelSourceNamespace:           r.obj.Namespace,
	}
}

func (r *ApplicationRelease) HelmChartLabels() map[string]string {
	labels := r.SourceLabels()
	ref := r.ChartRef()

	switch ref.Repository.Kind {
	case helmv1alpha1.HelmApplicationRepositoryKind:
		labels[helmv1alpha1.HelmApplicationChartLabelSourceName] = naming.ApplicationChartName(ref.Repository.Name, ref.Chart)
	case helmv1alpha1.HelmClusterApplicationRepositoryKind:
		labels[helmv1alpha1.HelmClusterApplicationChartLabelSourceName] = naming.ClusterApplicationChartName(ref.Repository.Name, ref.Chart)
	}

	return labels
}

func (r *ApplicationRelease) InternalNames() source.ReleaseNames {
	name := utils.DerivedName(applicationPrefix, helmv1alpha1.HelmApplicationKind, r.obj.Namespace, r.obj.Name)

	return source.ReleaseNames{
		HelmChart:      name,
		HelmRelease:    name,
		OCIRepository:  name,
		ServiceAccount: name,
	}
}

func (r *ApplicationRelease) LastAppliedChart() *source.ChartRef {
	last := r.obj.Status.LastAppliedChart
	if last == nil || last.RepositoryKind() == "" {
		return nil
	}

	return &source.ChartRef{
		Repository: r.repositoryRef(last.RepositoryKind(), last.RepositoryName()),
		Chart:      last.Name,
		Version:    last.Version,
	}
}

// SetLastAppliedChart replaces the record wholesale. A merge would leave the old
// repository field set next to the new clusterRepository, both fields would be
// filled, and IsChartStatusInfoOutdated would stay true forever.
func (r *ApplicationRelease) SetLastAppliedChart(ref source.ChartRef) {
	last := &helmv1alpha1.HelmApplicationLastAppliedChartRef{
		Name:    ref.Chart,
		Version: ref.Version,
	}

	switch ref.Repository.Kind {
	case helmv1alpha1.HelmApplicationRepositoryKind:
		last.Repository = ref.Repository.Name
	case helmv1alpha1.HelmClusterApplicationRepositoryKind:
		last.ClusterRepository = ref.Repository.Name
	}

	r.obj.Status.LastAppliedChart = last
}

func (r *ApplicationRelease) SetLastAppliedValues(values *apiextensionsv1.JSON) {
	r.obj.Status.LastAppliedValues = values
}

func (r *ApplicationRelease) SetLastForceReconcileTime(t metav1.Time) {
	r.obj.Status.LastForceReconcileTime = &t
}

// applicationRepositoryResolver loads whichever of the two repository kinds an
// application references, with the catalog of that kind.
type applicationRepositoryResolver struct {
	client         client.Client
	namespaced     source.Catalog
	clusterCatalog source.Catalog
}

func NewApplicationRepositoryResolver(c client.Client) source.RepositoryResolver {
	return &applicationRepositoryResolver{
		client:         c,
		namespaced:     NewApplicationCatalog(c),
		clusterCatalog: NewClusterApplicationCatalog(c),
	}
}

func (r *applicationRepositoryResolver) Resolve(ctx context.Context, ref source.RepositoryRef) (source.Repository, source.Catalog, error) {
	switch ref.Kind {
	case helmv1alpha1.HelmApplicationRepositoryKind:
		repo := &helmv1alpha1.HelmApplicationRepository{}
		if err := r.client.Get(ctx, client.ObjectKey{Namespace: ref.Namespace, Name: ref.Name}, repo); err != nil {
			return nil, nil, fmt.Errorf("getting %s %s/%s: %w", ref.Kind, ref.Namespace, ref.Name, err)
		}

		return NewApplicationRepository(repo), r.namespaced, nil
	case helmv1alpha1.HelmClusterApplicationRepositoryKind:
		repo := &helmv1alpha1.HelmClusterApplicationRepository{}
		if err := r.client.Get(ctx, client.ObjectKey{Name: ref.Name}, repo); err != nil {
			return nil, nil, fmt.Errorf("getting %s %q: %w", ref.Kind, ref.Name, err)
		}

		return NewClusterApplicationRepository(repo), r.clusterCatalog, nil
	default:
		// The CEL rule on spec.chart keeps exactly one reference set on any persisted
		// object, so this is reachable only for an object built in memory.
		return nil, nil, fmt.Errorf("unsupported repository kind %q", ref.Kind)
	}
}

// ListApplicationReleases lists the HelmApplication objects consuming a repository,
// or only those consuming one of its charts. The index value carries the repository
// kind and namespace, so a namespaced repository only ever sees the applications of
// its own namespace.
func ListApplicationReleases(c client.Client) source.ReleaseLister {
	return func(ctx context.Context, repo source.Repository, chartName string) ([]source.Release, error) {
		kind := repo.OwnerGVK().Kind

		selector := client.MatchingFields{index.ApplicationRepository: index.ApplicationRepositoryValue(kind, repo.Namespace(), repo.Name())}
		if chartName != "" {
			selector = client.MatchingFields{index.ApplicationChart: index.ApplicationChartValue(kind, repo.Namespace(), repo.Name(), chartName)}
		}

		var apps helmv1alpha1.HelmApplicationList
		if err := c.List(ctx, &apps, selector); err != nil {
			return nil, fmt.Errorf("listing applications of repository %s %s/%s: %w", kind, repo.Namespace(), repo.Name(), err)
		}

		releases := make([]source.Release, 0, len(apps.Items))
		for i := range apps.Items {
			releases = append(releases, NewApplicationRelease(&apps.Items[i]))
		}

		return releases, nil
	}
}
