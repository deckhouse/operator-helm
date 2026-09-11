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

package resolver

import (
	"context"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/client"

	apinaming "github.com/deckhouse/operator-helm/api/naming"
	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
)

// repositorySpec is what resolving a chart needs from a repository, whichever kind
// it is: where the charts come from and how to reach them.
type repositorySpec struct {
	URL                string
	Auth               *helmv1alpha1.RepositoryAuth
	CACertificate      string
	InsecureSkipVerify bool
}

// repositoryFamily is everything that differs between the repository kinds: how the
// repository and its chart catalog are read, and how the internal objects the
// operator derived from that repository are recognised. Namespaced says whether a
// request must carry a namespace.
//
// The functions take the client rather than closing over one because the resolver
// owns a single client and a family is a value, not a service.
type repositoryFamily struct {
	Kind       RepositoryKind
	Namespaced bool

	// GetRepository reads the repository. A NotFound error means the caller should
	// report repository_not_found.
	GetRepository func(ctx context.Context, c client.Client, namespace, name string) (*repositorySpec, error)
	// ChartVersions reads the catalog entry of one chart. A NotFound error means the
	// catalog has not caught up yet and the caller should report pending.
	ChartVersions func(ctx context.Context, c client.Client, namespace, repository, chart string) ([]helmv1alpha1.ChartVersion, error)
	// InternalLabels selects the internal objects operator-helm-controller derived
	// from this repository — its HelmRepository and its auth/TLS secrets, all of
	// which live in the operator namespace and carry the source labels of their kind.
	InternalLabels func(namespace, repository string) map[string]string
}

// families is the registry every supported repository kind is looked up in.
var families = map[RepositoryKind]repositoryFamily{
	RepositoryKindHelmClusterAddon: {
		Kind:       RepositoryKindHelmClusterAddon,
		Namespaced: false,
		GetRepository: func(ctx context.Context, c client.Client, _, name string) (*repositorySpec, error) {
			repo := &helmv1alpha1.HelmClusterAddonRepository{}
			if err := c.Get(ctx, client.ObjectKey{Name: name}, repo); err != nil {
				return nil, err
			}

			return specOf(repo.Spec), nil
		},
		ChartVersions: func(ctx context.Context, c client.Client, _, repository, chart string) ([]helmv1alpha1.ChartVersion, error) {
			obj := &helmv1alpha1.HelmClusterAddonChart{}
			key := client.ObjectKey{Name: apinaming.HelmClusterAddonChartName(repository, chart)}
			if err := c.Get(ctx, key, obj); err != nil {
				return nil, err
			}

			return obj.Status.Versions, nil
		},
		InternalLabels: func(_, repository string) map[string]string {
			return map[string]string{helmv1alpha1.HelmClusterAddonRepositoryLabelSourceName: repository}
		},
	},
	RepositoryKindHelmApplication: {
		Kind:       RepositoryKindHelmApplication,
		Namespaced: true,
		GetRepository: func(ctx context.Context, c client.Client, namespace, name string) (*repositorySpec, error) {
			repo := &helmv1alpha1.HelmApplicationRepository{}
			if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, repo); err != nil {
				return nil, err
			}

			return specOf(repo.Spec), nil
		},
		ChartVersions: func(ctx context.Context, c client.Client, namespace, repository, chart string) ([]helmv1alpha1.ChartVersion, error) {
			obj := &helmv1alpha1.HelmApplicationChart{}
			key := client.ObjectKey{Namespace: namespace, Name: apinaming.ApplicationChartName(repository, chart)}
			if err := c.Get(ctx, key, obj); err != nil {
				return nil, err
			}

			return obj.Status.Versions, nil
		},
		InternalLabels: func(namespace, repository string) map[string]string {
			return map[string]string{
				helmv1alpha1.HelmApplicationRepositoryLabelSourceName: repository,
				helmv1alpha1.LabelSourceNamespace:                     namespace,
			}
		},
	},
	RepositoryKindHelmClusterApplication: {
		Kind:       RepositoryKindHelmClusterApplication,
		Namespaced: false,
		GetRepository: func(ctx context.Context, c client.Client, _, name string) (*repositorySpec, error) {
			repo := &helmv1alpha1.HelmClusterApplicationRepository{}
			if err := c.Get(ctx, client.ObjectKey{Name: name}, repo); err != nil {
				return nil, err
			}

			return specOf(repo.Spec), nil
		},
		ChartVersions: func(ctx context.Context, c client.Client, _, repository, chart string) ([]helmv1alpha1.ChartVersion, error) {
			obj := &helmv1alpha1.HelmClusterApplicationChart{}
			key := client.ObjectKey{Name: apinaming.ClusterApplicationChartName(repository, chart)}
			if err := c.Get(ctx, key, obj); err != nil {
				return nil, err
			}

			return obj.Status.Versions, nil
		},
		InternalLabels: func(_, repository string) map[string]string {
			return map[string]string{helmv1alpha1.HelmClusterApplicationRepositoryLabelSourceName: repository}
		},
	},
}

// familyFor looks a kind up. The kind is already lower-cased by Resolve.
func familyFor(kind RepositoryKind) (repositoryFamily, bool) {
	family, ok := families[kind]

	return family, ok
}

func specOf(spec helmv1alpha1.RepositorySpec) *repositorySpec {
	return &repositorySpec{
		URL:                spec.URL,
		Auth:               spec.Auth,
		CACertificate:      spec.CACertificate,
		InsecureSkipVerify: spec.InsecureSkipVerify,
	}
}

// requireNamespace reports the request-shape error of a family: a namespaced kind
// needs a namespace to identify its repository. A cluster-scoped kind accepts any
// namespace, including a non-empty one: for such a kind the namespace is not part of
// the chart's identity but the caller's authorization context (e.g. the namespace it
// intends to create a HelmApplication in), which this resolver does not use.
func (f repositoryFamily) requireNamespace(namespace string) error {
	if f.Namespaced && namespace == "" {
		return fmt.Errorf("repository kind %q is namespaced: namespace is required", f.Kind)
	}

	return nil
}
