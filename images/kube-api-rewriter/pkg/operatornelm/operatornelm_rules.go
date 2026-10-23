/*
Copyright 2024 Flant JSC

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

package operatornelm

import (
	. "github.com/deckhouse/kube-api-rewriter/pkg/rewriter"
)

const (
	internalPrefix = "internal.operator-helm.deckhouse.io"
)

var OperatorNelmRewriteRules = &RewriteRules{
	KindPrefix:         "InternalNelmOperator",
	ResourceTypePrefix: "internalnelmoperator",
	Rules:              OperatorNelmAPIGroupsRules,
	Webhooks:           OperatorNelmWebhooks,
	Labels: MetadataReplace{
		Names: []MetadataReplaceRule{
			{Original: "source.toolkit.fluxcd.io", Renamed: "source." + internalPrefix},
			{Original: "helm.toolkit.fluxcd.io", Renamed: "helm." + internalPrefix},
		},
		Prefixes: []MetadataReplaceRule{
			{Original: "source.toolkit.fluxcd.io", Renamed: "source." + internalPrefix},
			{Original: "helm.toolkit.fluxcd.io", Renamed: "helm." + internalPrefix},
		},
	},
	Annotations: MetadataReplace{
		Names: []MetadataReplaceRule{
			{Original: "reconcile.fluxcd.io/requestedAt", Renamed: "reconcile." + internalPrefix + "/requestedAt"},
			{Original: "reconcile.fluxcd.io/forceAt", Renamed: "reconcile." + internalPrefix + "/forceAt"},
			// Unlike its siblings, resetAt is never written by this module: no
			// live object carries it, so there is nothing stored to preserve.
			// It joins them in the internal namespace instead of freezing on
			// the fork's domain.
			{Original: "reconcile.fluxcd.io/resetAt", Renamed: "reconcile." + internalPrefix + "/resetAt"},
		},
		Prefixes: []MetadataReplaceRule{
			{Original: "source.toolkit.fluxcd.io", Renamed: "source." + internalPrefix},
			{Original: "helm.toolkit.fluxcd.io", Renamed: "helm." + internalPrefix},
		},
	},
	Finalizers: MetadataReplace{
		Names: []MetadataReplaceRule{
			{Original: "finalizers.fluxcd.io", Renamed: "finalizers." + internalPrefix},
		},
	},
	Excludes: []ExcludeRule{},
	KindRefPaths: map[string][]string{
		"HelmChart":   {"spec.sourceRef"},
		"HelmRelease": {"spec.chart.spec.sourceRef", "spec.chartRef"},
	},
}

var OperatorNelmAPIGroupsRules = map[string]APIGroupRule{
	"source.toolkit.fluxcd.io": {
		GroupRule: GroupRule{
			Group:            "source.toolkit.fluxcd.io",
			Versions:         []string{"v1"},
			PreferredVersion: "v1",
			Renamed:          "source." + internalPrefix,
		},
		ResourceRules: map[string]ResourceRule{
			"buckets": {
				Kind:             "Bucket",
				ListKind:         "BucketList",
				Plural:           "buckets",
				Singular:         "bucket",
				Versions:         []string{"v1"},
				PreferredVersion: "v1",
				Categories:       []string{},
				ShortNames:       []string{},
			},
			"externalartifacts": {
				Kind:             "ExternalArtifact",
				ListKind:         "ExternalArtifactList",
				Plural:           "externalartifacts",
				Singular:         "externalartifact",
				Versions:         []string{"v1"},
				PreferredVersion: "v1",
				Categories:       []string{},
				ShortNames:       []string{},
			},
			"gitrepositories": {
				Kind:             "GitRepository",
				ListKind:         "GitRepositoryList",
				Plural:           "gitrepositories",
				Singular:         "gitrepository",
				Versions:         []string{"v1"},
				PreferredVersion: "v1",
				Categories:       []string{},
				ShortNames:       []string{},
			},
			"helmcharts": {
				Kind:             "HelmChart",
				ListKind:         "HelmChartList",
				Plural:           "helmcharts",
				Singular:         "helmchart",
				Versions:         []string{"v1"},
				PreferredVersion: "v1",
				Categories:       []string{},
				ShortNames:       []string{},
			},
			"helmrepositories": {
				Kind:             "HelmRepository",
				ListKind:         "HelmRepositoryList",
				Plural:           "helmrepositories",
				Singular:         "helmrepository",
				Versions:         []string{"v1"},
				PreferredVersion: "v1",
				Categories:       []string{},
				ShortNames:       []string{},
			},
			"ocirepositories": {
				Kind:             "OCIRepository",
				ListKind:         "OCIRepositoryList",
				Plural:           "ocirepositories",
				Singular:         "ocirepository",
				Versions:         []string{"v1"},
				PreferredVersion: "v1",
				Categories:       []string{},
				ShortNames:       []string{},
			},
		},
	},
	"helm.toolkit.fluxcd.io": {
		GroupRule: GroupRule{
			Group:            "helm.toolkit.fluxcd.io",
			Versions:         []string{"v2"},
			PreferredVersion: "v2",
			Renamed:          "helm." + internalPrefix,
		},
		ResourceRules: map[string]ResourceRule{
			"helmreleases": {
				Kind:             "HelmRelease",
				ListKind:         "HelmReleaseList",
				Plural:           "helmreleases",
				Singular:         "helmrelease",
				Versions:         []string{"v2"},
				PreferredVersion: "v2",
				Categories:       []string{},
				ShortNames:       []string{},
			},
		},
	},
}

var OperatorNelmWebhooks = map[string]WebhookRule{}
