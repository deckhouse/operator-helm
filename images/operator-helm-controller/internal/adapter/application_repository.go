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

// Prefixes of the internal objects derived from a HelmApplicationRepository. The
// "ap" stands for application and keeps the family apart from the addon prefixes
// hca/hcar, which are frozen.
const (
	applicationRepositoryPrefix     = "hapr"
	applicationRepositoryAuthPrefix = "hapr-auth"
	applicationRepositoryTLSPrefix  = "hapr-tls"
)

var _ source.Repository = (*ApplicationRepository)(nil)

// ApplicationRepository adapts a HelmApplicationRepository. The kind is
// namespaced while its internal objects live in the operator namespace, so both
// the labels and the derived names carry the repository's namespace.
type ApplicationRepository struct {
	obj *helmv1alpha1.HelmApplicationRepository
}

func NewApplicationRepository(obj *helmv1alpha1.HelmApplicationRepository) *ApplicationRepository {
	return &ApplicationRepository{obj: obj}
}

func EmptyApplicationRepository() source.Repository {
	return NewApplicationRepository(&helmv1alpha1.HelmApplicationRepository{})
}

func (r *ApplicationRepository) Object() status.ObjectWithConditions { return r.obj }
func (r *ApplicationRepository) Name() string                        { return r.obj.Name }
func (r *ApplicationRepository) Namespace() string                   { return r.obj.Namespace }
func (r *ApplicationRepository) Generation() int64                   { return r.obj.Generation }

func (r *ApplicationRepository) OwnerGVK() schema.GroupVersionKind {
	return helmv1alpha1.HelmApplicationRepositoryGVK
}

func (r *ApplicationRepository) URL() string                        { return r.obj.Spec.URL }
func (r *ApplicationRepository) Auth() *helmv1alpha1.RepositoryAuth { return r.obj.Spec.Auth }
func (r *ApplicationRepository) CACertificate() string              { return r.obj.Spec.CACertificate }

func (r *ApplicationRepository) InsecureSkipVerify() bool               { return r.obj.Spec.InsecureSkipVerify }
func (r *ApplicationRepository) Status() *helmv1alpha1.RepositoryStatus { return &r.obj.Status }
func (r *ApplicationRepository) ForceReconcileRequired() bool           { return r.obj.ForceReconcileRequired() }

func (r *ApplicationRepository) SourceLabels() map[string]string {
	return map[string]string{
		helmv1alpha1.LabelManagedBy:                           helmv1alpha1.LabelManagedByValue,
		helmv1alpha1.HelmApplicationRepositoryLabelSourceName: r.obj.Name,
		helmv1alpha1.LabelSourceNamespace:                     r.obj.Namespace,
	}
}

func (r *ApplicationRepository) InternalNames() source.InternalNames {
	kind := helmv1alpha1.HelmApplicationRepositoryKind

	return source.InternalNames{
		HelmRepository: utils.DerivedName(applicationRepositoryPrefix, kind, r.obj.Namespace, r.obj.Name),
		AuthSecret:     utils.DerivedName(applicationRepositoryAuthPrefix, kind, r.obj.Namespace, r.obj.Name),
		TLSSecret:      utils.DerivedName(applicationRepositoryTLSPrefix, kind, r.obj.Namespace, r.obj.Name),
	}
}
