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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	apinaming "github.com/deckhouse/operator-helm/api/naming"
	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
)

type repositorySpec struct {
	URL                string
	Auth               *helmv1alpha1.RepositoryAuth
	CACertificate      string
	InsecureSkipVerify bool

	// Identity is which repository object this is, not what it is called; see identityOf.
	Identity string
}

type repositoryFamily struct {
	Kind       RepositoryKind
	Namespaced bool

	// GetRepository reads the repository. A NotFound error means the caller should
	// report repository_not_found.
	GetRepository func(ctx context.Context, c client.Client, namespace, name string) (*repositorySpec, error)
	// ChartVersions reads the catalog entry of one chart. A NotFound error means the
	// catalog has not caught up yet and the caller should report pending.
	ChartVersions func(ctx context.Context, c client.Client, namespace, repository, chart string) ([]helmv1alpha1.ChartVersion, error)
	// InternalLabels selects the internal objects operator-helm-controller derived from
	// this repository: its HelmRepository and its auth/TLS secrets.
	InternalLabels func(namespace, repository string) map[string]string
}

var families = map[RepositoryKind]repositoryFamily{
	RepositoryKindHelmClusterAddon: {
		Kind:       RepositoryKindHelmClusterAddon,
		Namespaced: false,
		GetRepository: func(ctx context.Context, c client.Client, _, name string) (*repositorySpec, error) {
			repo := &helmv1alpha1.HelmClusterAddonRepository{}
			if err := c.Get(ctx, client.ObjectKey{Name: name}, repo); err != nil {
				return nil, err
			}

			return specOf(repo.ObjectMeta, repo.Spec), nil
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

			return specOf(repo.ObjectMeta, repo.Spec), nil
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

			return specOf(repo.ObjectMeta, repo.Spec), nil
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

func familyFor(kind RepositoryKind) (repositoryFamily, bool) {
	family, ok := families[kind]

	return family, ok
}

func specOf(meta metav1.ObjectMeta, spec helmv1alpha1.RepositorySpec) *repositorySpec {
	return &repositorySpec{
		URL:                spec.URL,
		Auth:               spec.Auth,
		CACertificate:      spec.CACertificate,
		InsecureSkipVerify: spec.InsecureSkipVerify,
		Identity:           identityOf(meta),
	}
}

// identityOf names a repository object by UID and generation, unmoved by status and catalog writes.
func identityOf(meta metav1.ObjectMeta) string {
	return fmt.Sprintf("%s@%d", meta.UID, meta.Generation)
}

func (f repositoryFamily) requireNamespace(namespace string) error {
	if f.Namespaced && namespace == "" {
		return fmt.Errorf("repository kind %q is namespaced: namespace is required", f.Kind)
	}

	return nil
}
