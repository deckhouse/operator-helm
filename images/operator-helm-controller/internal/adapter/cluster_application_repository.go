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

// Prefixes of the internal objects derived from a HelmClusterApplicationRepository.
const (
	clusterApplicationRepositoryPrefix     = "hcapr"
	clusterApplicationRepositoryAuthPrefix = "hcapr-auth"
	clusterApplicationRepositoryTLSPrefix  = "hcapr-tls"
)

var _ source.Repository = (*ClusterApplicationRepository)(nil)

// ClusterApplicationRepository adapts a HelmClusterApplicationRepository. It is
// cluster-scoped, so its derived names carry no namespace part, and its labels no
// namespace label.
type ClusterApplicationRepository struct {
	obj *helmv1alpha1.HelmClusterApplicationRepository
}

func NewClusterApplicationRepository(obj *helmv1alpha1.HelmClusterApplicationRepository) *ClusterApplicationRepository {
	return &ClusterApplicationRepository{obj: obj}
}

func EmptyClusterApplicationRepository() source.Repository {
	return NewClusterApplicationRepository(&helmv1alpha1.HelmClusterApplicationRepository{})
}

func (r *ClusterApplicationRepository) Object() status.ObjectWithConditions { return r.obj }
func (r *ClusterApplicationRepository) Name() string                        { return r.obj.Name }
func (r *ClusterApplicationRepository) Namespace() string                   { return r.obj.Namespace }
func (r *ClusterApplicationRepository) Generation() int64                   { return r.obj.Generation }

func (r *ClusterApplicationRepository) OwnerGVK() schema.GroupVersionKind {
	return helmv1alpha1.HelmClusterApplicationRepositoryGVK
}

func (r *ClusterApplicationRepository) URL() string                        { return r.obj.Spec.URL }
func (r *ClusterApplicationRepository) Auth() *helmv1alpha1.RepositoryAuth { return r.obj.Spec.Auth }
func (r *ClusterApplicationRepository) CACertificate() string              { return r.obj.Spec.CACertificate }
func (r *ClusterApplicationRepository) InsecureSkipVerify() bool {
	return r.obj.Spec.InsecureSkipVerify
}

func (r *ClusterApplicationRepository) Status() *helmv1alpha1.RepositoryStatus {
	return &r.obj.Status
}

func (r *ClusterApplicationRepository) ForceReconcileRequired() bool {
	return r.obj.ForceReconcileRequired()
}

func (r *ClusterApplicationRepository) SourceLabels() map[string]string {
	return map[string]string{
		helmv1alpha1.LabelManagedBy:                                  helmv1alpha1.LabelManagedByValue,
		helmv1alpha1.HelmClusterApplicationRepositoryLabelSourceName: r.obj.Name,
	}
}

func (r *ClusterApplicationRepository) InternalNames() source.InternalNames {
	kind := helmv1alpha1.HelmClusterApplicationRepositoryKind

	return source.InternalNames{
		HelmRepository: utils.DerivedName(clusterApplicationRepositoryPrefix, kind, "", r.obj.Name),
		AuthSecret:     utils.DerivedName(clusterApplicationRepositoryAuthPrefix, kind, "", r.obj.Name),
		TLSSecret:      utils.DerivedName(clusterApplicationRepositoryTLSPrefix, kind, "", r.obj.Name),
	}
}
