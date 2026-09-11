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
	"k8s.io/apimachinery/pkg/runtime/schema"

	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
	"github.com/deckhouse/operator-helm/internal/manager/status"
	"github.com/deckhouse/operator-helm/internal/source"
	"github.com/deckhouse/operator-helm/internal/utils"
)

var _ source.Repository = (*AddonRepository)(nil)

// AddonRepository adapts a HelmClusterAddonRepository. Its labels and internal
// names are exactly what the addon controller has always written: those are live
// objects, and the frozen naming functions in utils keep them in place.
type AddonRepository struct {
	obj *helmv1alpha1.HelmClusterAddonRepository
}

func NewAddonRepository(obj *helmv1alpha1.HelmClusterAddonRepository) *AddonRepository {
	return &AddonRepository{obj: obj}
}

// EmptyAddonRepository returns an adapter around a zero object, for the reconciler
// to read the API object into.
func EmptyAddonRepository() source.Repository {
	return NewAddonRepository(&helmv1alpha1.HelmClusterAddonRepository{})
}

func (r *AddonRepository) Object() status.ObjectWithConditions { return r.obj }
func (r *AddonRepository) Name() string                        { return r.obj.Name }
func (r *AddonRepository) Namespace() string                   { return r.obj.Namespace }
func (r *AddonRepository) Generation() int64                   { return r.obj.Generation }

func (r *AddonRepository) OwnerGVK() schema.GroupVersionKind {
	return helmv1alpha1.HelmClusterAddonRepositoryGVK
}

func (r *AddonRepository) URL() string                        { return r.obj.Spec.URL }
func (r *AddonRepository) Auth() *helmv1alpha1.RepositoryAuth { return r.obj.Spec.Auth }
func (r *AddonRepository) CACertificate() string              { return r.obj.Spec.CACertificate }

func (r *AddonRepository) InsecureSkipVerify() bool               { return r.obj.Spec.InsecureSkipVerify }
func (r *AddonRepository) Status() *helmv1alpha1.RepositoryStatus { return &r.obj.Status }
func (r *AddonRepository) ForceReconcileRequired() bool           { return r.obj.ForceReconcileRequired() }

func (r *AddonRepository) SourceLabels() map[string]string {
	return map[string]string{
		helmv1alpha1.LabelManagedBy:                            helmv1alpha1.LabelManagedByValue,
		helmv1alpha1.HelmClusterAddonRepositoryLabelSourceName: r.obj.Name,
	}
}

func (r *AddonRepository) InternalNames() source.InternalNames {
	return source.InternalNames{
		HelmRepository: utils.GetInternalHelmRepositoryName(r.obj.Name),
		AuthSecret:     utils.GetInternalRepositoryAuthSecretName(r.obj.Name),
		TLSSecret:      utils.GetInternalRepositoryTLSSecretName(r.obj.Name),
	}
}
