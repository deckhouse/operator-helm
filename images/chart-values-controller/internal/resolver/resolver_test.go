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
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/deckhouse/operator-helm/api/naming"
	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
)

func newTestResolver(t *testing.T, objects ...client.Object) *Resolver {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("registering client-go scheme: %v", err)
	}
	if err := helmv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("registering helm scheme: %v", err)
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()

	return &Resolver{client: c}
}

func chartWithVersions(repoName, chartName string, versions ...helmv1alpha1.HelmClusterAddonChartVersion) *helmv1alpha1.HelmClusterAddonChart {
	return &helmv1alpha1.HelmClusterAddonChart{
		ObjectMeta: metav1.ObjectMeta{Name: naming.HelmClusterAddonChartName(repoName, chartName)},
		Status:     helmv1alpha1.HelmClusterAddonChartStatus{Versions: versions},
	}
}

func TestChartVersionMediaType(t *testing.T) {
	req := Request{Kind: RepositoryKindHelmClusterAddon, RepositoryName: "example", Chart: "podinfo", Version: "6.7.1"}

	t.Run("a usable version returns its media type", func(t *testing.T) {
		resolver := newTestResolver(t, chartWithVersions("example", "podinfo",
			helmv1alpha1.HelmClusterAddonChartVersion{Version: "6.7.1", MediaType: "application/tar+gzip"},
		))

		mediaType, done, err := resolver.chartVersionMediaType(context.Background(), req)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if done != nil {
			t.Fatalf("expected to continue, got outcome %q", done.Outcome)
		}
		if mediaType != "application/tar+gzip" {
			t.Fatalf("media type is %q", mediaType)
		}
	})

	t.Run("a missing chart is pending", func(t *testing.T) {
		resolver := newTestResolver(t)

		_, done, err := resolver.chartVersionMediaType(context.Background(), req)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if done == nil || done.Outcome != OutcomePending {
			t.Fatalf("outcome is %+v, want pending", done)
		}
	})

	t.Run("a missing version is pending", func(t *testing.T) {
		resolver := newTestResolver(t, chartWithVersions("example", "podinfo",
			helmv1alpha1.HelmClusterAddonChartVersion{Version: "6.7.0", MediaType: "application/tar+gzip"},
		))

		_, done, err := resolver.chartVersionMediaType(context.Background(), req)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if done == nil || done.Outcome != OutcomePending {
			t.Fatalf("outcome is %+v, want pending", done)
		}
	})

	t.Run("an unusable version is values_not_found with the reason", func(t *testing.T) {
		resolver := newTestResolver(t, chartWithVersions("example", "podinfo",
			helmv1alpha1.HelmClusterAddonChartVersion{
				Version:            "6.7.1",
				UnavailableReason:  helmv1alpha1.UnavailableReasonUnsupportedMediaType,
				UnavailableMessage: "config media type \"application/vnd.unknown.config.v1+json\" is not a helm chart config",
			},
		))

		_, done, err := resolver.chartVersionMediaType(context.Background(), req)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if done == nil || done.Outcome != OutcomeValuesNotFound {
			t.Fatalf("outcome is %+v, want values_not_found", done)
		}
		if !strings.Contains(done.Message, helmv1alpha1.UnavailableReasonUnsupportedMediaType) {
			t.Fatalf("message %q must name the reason", done.Message)
		}
	})
}
