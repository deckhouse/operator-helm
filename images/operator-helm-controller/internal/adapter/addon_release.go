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
	"github.com/deckhouse/operator-helm/internal/contract"
	"github.com/deckhouse/operator-helm/internal/index"
	"github.com/deckhouse/operator-helm/internal/status"
	"github.com/deckhouse/operator-helm/internal/utils"
)

var _ contract.Release = (*AddonRelease)(nil)

// AddonRelease adapts a HelmClusterAddon. Its labels and internal names are frozen to what
// the addon controller has always written; the release name is the addon name, bounded.
type AddonRelease struct {
	obj *helmv1alpha1.HelmClusterAddon
}

func NewAddonRelease(obj *helmv1alpha1.HelmClusterAddon) *AddonRelease {
	return &AddonRelease{obj: obj}
}

// EmptyAddonRelease returns an adapter around a zero object, for the reconciler to read the
// API object into.
func EmptyAddonRelease() contract.Release {
	return NewAddonRelease(&helmv1alpha1.HelmClusterAddon{})
}

func (r *AddonRelease) Object() status.ObjectWithConditions { return r.obj }
func (r *AddonRelease) Kind() string                        { return helmv1alpha1.HelmClusterAddonKind }
func (r *AddonRelease) Name() string                        { return r.obj.Name }
func (r *AddonRelease) Namespace() string                   { return r.obj.Namespace }
func (r *AddonRelease) Generation() int64                   { return r.obj.Generation }

func (r *AddonRelease) ChartRef() contract.ChartRef {
	return contract.ChartRef{
		Repository: contract.RepositoryRef{
			Kind: helmv1alpha1.HelmClusterAddonRepositoryKind,
			Name: r.obj.Spec.Chart.HelmClusterAddonRepository,
		},
		Chart:   r.obj.Spec.Chart.HelmClusterAddonChartName,
		Version: r.obj.Spec.Chart.Version,
	}
}

func (r *AddonRelease) TargetNamespace() string         { return r.obj.Spec.Namespace }
func (r *AddonRelease) Values() *apiextensionsv1.JSON   { return r.obj.Spec.Values }
func (r *AddonRelease) MaintenanceActivated() bool      { return r.obj.MaintenanceModeActivated() }
func (r *AddonRelease) MaintenanceEnabled() bool        { return r.obj.MaintenanceModeEnabled() }
func (r *AddonRelease) ForceReconcileRequired() bool    { return r.obj.ForceReconcileRequired() }
func (r *AddonRelease) ReleaseName() string             { return naming.HelmReleaseName(r.obj.Name) }
func (r *AddonRelease) IsChartStatusInfoOutdated() bool { return r.obj.IsChartStatusInfoOutdated() }

func (r *AddonRelease) SourceLabels() map[string]string {
	return map[string]string{
		helmv1alpha1.LabelManagedBy:                  helmv1alpha1.LabelManagedByValue,
		helmv1alpha1.HelmClusterAddonLabelSourceName: r.obj.Name,
	}
}

func (r *AddonRelease) HelmChartLabels() map[string]string {
	labels := r.SourceLabels()
	labels[helmv1alpha1.HelmClusterAddonChartLabelSourceName] = naming.HelmClusterAddonChartName(
		r.obj.Spec.Chart.HelmClusterAddonRepository, r.obj.Spec.Chart.HelmClusterAddonChartName,
	)

	return labels
}

func (r *AddonRelease) InternalNames() contract.ReleaseNames {
	return contract.ReleaseNames{
		HelmChart:     utils.GetInternalHelmChartName(r.obj.Name),
		HelmRelease:   utils.GetInternalHelmReleaseName(r.obj.Name),
		OCIRepository: utils.GetInternalOCIRepositoryName(r.obj.Name),
	}
}

func (r *AddonRelease) LastAppliedChart() *contract.ChartRef {
	last := r.obj.Status.LastAppliedChart
	if last == nil {
		return nil
	}

	return &contract.ChartRef{
		Repository: contract.RepositoryRef{
			Kind: helmv1alpha1.HelmClusterAddonRepositoryKind,
			Name: last.HelmClusterAddonRepository,
		},
		Chart:   last.HelmClusterAddonChartName,
		Version: last.Version,
	}
}

func (r *AddonRelease) SetLastAppliedChart(ref contract.ChartRef) {
	r.obj.Status.LastAppliedChart = &helmv1alpha1.HelmClusterAddonLastAppliedChartRef{
		HelmClusterAddonChartName:  ref.Chart,
		HelmClusterAddonRepository: ref.Repository.Name,
		Version:                    ref.Version,
	}
}

func (r *AddonRelease) SetLastAppliedValues(values *apiextensionsv1.JSON) {
	r.obj.Status.LastAppliedValues = values
}

func (r *AddonRelease) SetLastForceReconcileTime(t metav1.Time) {
	r.obj.Status.LastForceReconcileTime = &t
}

func (r *AddonRelease) SetInventory(entries []helmv1alpha1.InventoryEntry) {
	r.obj.Status.Inventory = entries
}

type AddonRepositoryResolver struct {
	client  client.Client
	catalog contract.Catalog
}

// NewAddonRepositoryResolver builds the resolver the release reconciler looks charts up
// through. Its catalog is only ever looked up in, so it reads through the cache.
func NewAddonRepositoryResolver(c client.Client) *AddonRepositoryResolver {
	return &AddonRepositoryResolver{client: c, catalog: NewAddonCatalog(c, c)}
}

func (r *AddonRepositoryResolver) Resolve(ctx context.Context, ref contract.RepositoryRef) (contract.Repository, contract.Catalog, error) {
	repo := &helmv1alpha1.HelmClusterAddonRepository{}
	if err := r.client.Get(ctx, client.ObjectKey{Name: ref.Name}, repo); err != nil {
		return nil, nil, fmt.Errorf("getting %s %q: %w", helmv1alpha1.HelmClusterAddonRepositoryKind, ref.Name, err)
	}

	return NewAddonRepository(repo), r.catalog, nil
}

// ListAddonReleases lists the HelmClusterAddon objects consuming a repository, or
// only those consuming one of its charts, through the two addon indexes.
func ListAddonReleases(c client.Client) contract.ReleaseLister {
	return func(ctx context.Context, repo contract.Repository, chartName string) ([]contract.Release, error) {
		selector := client.MatchingFields{index.AddonRepository: repo.Name()}
		if chartName != "" {
			selector = client.MatchingFields{index.AddonChart: index.AddonChartValue(repo.Name(), chartName)}
		}

		var addons helmv1alpha1.HelmClusterAddonList
		if err := c.List(ctx, &addons, selector); err != nil {
			return nil, fmt.Errorf("listing addons of repository %q: %w", repo.Name(), err)
		}

		releases := make([]contract.Release, 0, len(addons.Items))
		for i := range addons.Items {
			releases = append(releases, NewAddonRelease(&addons.Items[i]))
		}

		return releases, nil
	}
}
