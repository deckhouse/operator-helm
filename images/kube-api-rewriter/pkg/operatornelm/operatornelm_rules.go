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
	ShortNamePrefix:    "intnelm",
	Categories:         []string{"intnelm"},
	Rules:              OperatorNelmAPIGroupsRules,
	Webhooks:           OperatorNelmWebhooks,
	Labels: MetadataReplace{
		Names: []MetadataReplaceRule{
			{Original: "source.werf.io", Renamed: "source." + internalPrefix},
			{Original: "helm.werf.io", Renamed: "helm." + internalPrefix},
		},
		Prefixes: []MetadataReplaceRule{
			{Original: "source.werf.io", Renamed: "source." + internalPrefix},
			{Original: "helm.werf.io", Renamed: "helm." + internalPrefix},
		},
	},
	Annotations: MetadataReplace{
		Prefixes: []MetadataReplaceRule{
			{Original: "source.werf.io", Renamed: "source." + internalPrefix},
			{Original: "helm.werf.io", Renamed: "helm." + internalPrefix},
		},
	},
	Finalizers: MetadataReplace{
		Names: []MetadataReplaceRule{
			{Original: "finalizers.werf.io", Renamed: "finalizers." + internalPrefix},
		},
		Prefixes: []MetadataReplaceRule{
			{Original: "werf.io", Renamed: "werf." + internalPrefix},
		},
	},
	Excludes: []ExcludeRule{},
	KindRefPaths: map[string][]string{
		"HelmChart":   {"spec.sourceRef"},
		"HelmRelease": {"spec.chart.spec.sourceRef", "spec.chartRef"},
	},
}

var OperatorNelmAPIGroupsRules = map[string]APIGroupRule{
	"source.werf.io": {
		GroupRule: GroupRule{
			Group:            "source.werf.io",
			Versions:         []string{"v1beta1", "v1beta2", "v1"},
			PreferredVersion: "v1",
			Renamed:          "source." + internalPrefix,
		},
		ResourceRules: map[string]ResourceRule{
			"buckets": {
				Kind:             "Bucket",
				ListKind:         "BucketList",
				Plural:           "buckets",
				Singular:         "bucket",
				Versions:         []string{"v1beta2", "v1"},
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
				Versions:         []string{"v1beta2", "v1"},
				PreferredVersion: "v1",
				Categories:       []string{},
				ShortNames:       []string{"gitrepo"},
			},
			"helmcharts": {
				Kind:             "HelmChart",
				ListKind:         "HelmChartList",
				Plural:           "helmcharts",
				Singular:         "helmchart",
				Versions:         []string{"v1beta2", "v1"},
				PreferredVersion: "v1",
				Categories:       []string{},
				ShortNames:       []string{"hc"},
			},
			"helmrepositories": {
				Kind:             "HelmRepository",
				ListKind:         "HelmRepositoryList",
				Plural:           "helmrepositories",
				Singular:         "helmrepository",
				Versions:         []string{"v1beta2", "v1"},
				PreferredVersion: "v1",
				Categories:       []string{},
				ShortNames:       []string{"helmrepo"},
			},
			"ocirepositories": {
				Kind:             "OCIRepository",
				ListKind:         "OCIRepositoryList",
				Plural:           "ocirepositories",
				Singular:         "ocirepository",
				Versions:         []string{"v1beta2", "v1"},
				PreferredVersion: "v1",
				Categories:       []string{},
				ShortNames:       []string{"ocirepo"},
			},
		},
	},
	"helm.werf.io": {
		GroupRule: GroupRule{
			Group:            "helm.werf.io",
			Versions:         []string{"v2beta1", "v2beta2", "v2"},
			PreferredVersion: "v2",
			Renamed:          "helm." + internalPrefix,
		},
		ResourceRules: map[string]ResourceRule{
			"helmreleases": {
				Kind:             "HelmRelease",
				ListKind:         "HelmReleaseList",
				Plural:           "helmreleases",
				Singular:         "helmrelease",
				Versions:         []string{"v2beta2", "v2"},
				PreferredVersion: "v2",
				Categories:       []string{},
				ShortNames:       []string{"hr"},
			},
		},
	},
}

var OperatorNelmWebhooks = map[string]WebhookRule{}
