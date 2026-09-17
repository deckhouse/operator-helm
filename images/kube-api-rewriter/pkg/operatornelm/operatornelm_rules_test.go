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
	"fmt"
	"reflect"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

func TestOperatorNelmRulesToYAML(t *testing.T) {
	b, err := yaml.Marshal(OperatorNelmRewriteRules)
	if err != nil {
		t.Fatalf("should marshal operatornelm rules without error: %v", err)
	}

	fmt.Printf("%s\n", string(b))
}

func TestRulesMapUpstreamGroups(t *testing.T) {
	source, ok := OperatorNelmAPIGroupsRules["source.toolkit.fluxcd.io"]
	if !ok {
		t.Fatal("the upstream source group has no rule")
	}
	if source.GroupRule.Renamed != "source.internal.operator-helm.deckhouse.io" {
		t.Fatalf("source group renamed to %q", source.GroupRule.Renamed)
	}

	helm, ok := OperatorNelmAPIGroupsRules["helm.toolkit.fluxcd.io"]
	if !ok {
		t.Fatal("the upstream helm group has no rule")
	}
	if helm.GroupRule.Renamed != "helm.internal.operator-helm.deckhouse.io" {
		t.Fatalf("helm group renamed to %q", helm.GroupRule.Renamed)
	}

	if _, ok := OperatorNelmAPIGroupsRules["source.werf.io"]; ok {
		t.Fatal("the fork group is still mapped")
	}
	if _, ok := OperatorNelmAPIGroupsRules["helm.werf.io"]; ok {
		t.Fatal("the fork group is still mapped")
	}
}

// TestRulesServeOneVersionPerKind pins what upstream actually serves at the
// pinned tags: the beta versions are gone, and declaring one the api server does
// not know makes discovery answer for a version nothing can serve.
func TestRulesServeOneVersionPerKind(t *testing.T) {
	source := OperatorNelmAPIGroupsRules["source.toolkit.fluxcd.io"]
	if !reflect.DeepEqual(source.GroupRule.Versions, []string{"v1"}) {
		t.Fatalf("source versions = %v, want [v1]", source.GroupRule.Versions)
	}

	for name, rule := range source.ResourceRules {
		if !reflect.DeepEqual(rule.Versions, []string{"v1"}) {
			t.Fatalf("%s versions = %v, want [v1]", name, rule.Versions)
		}
	}

	helm := OperatorNelmAPIGroupsRules["helm.toolkit.fluxcd.io"]
	if !reflect.DeepEqual(helm.GroupRule.Versions, []string{"v2"}) {
		t.Fatalf("helm versions = %v, want [v2]", helm.GroupRule.Versions)
	}
}

// TestMetadataRenamesKeepTheStoredSide pins the constraint the whole migration
// rests on: objects already live in clusters, so what is written into them must
// not move. Only the left side of the table follows upstream.
func TestMetadataRenamesKeepTheStoredSide(t *testing.T) {
	const internal = "internal.operator-helm.deckhouse.io"

	wantAnnotations := map[string]string{
		"reconcile.fluxcd.io/requestedAt": "reconcile." + internal + "/requestedAt",
		"reconcile.fluxcd.io/forceAt":     "reconcile." + internal + "/forceAt",
		// The fork never had a rule renaming resetAt, so it already reaches
		// clusters under the fork's own domain: keep that stored form.
		"reconcile.fluxcd.io/resetAt": "reconcile.werf.io/resetAt",
	}
	got := map[string]string{}
	for _, rule := range OperatorNelmRewriteRules.Annotations.Names {
		got[rule.Original] = rule.Renamed
	}
	if !reflect.DeepEqual(got, wantAnnotations) {
		t.Fatalf("annotation names = %v, want %v", got, wantAnnotations)
	}

	wantFinalizers := map[string]string{
		"finalizers.fluxcd.io": "finalizers." + internal,
	}
	got = map[string]string{}
	for _, rule := range OperatorNelmRewriteRules.Finalizers.Names {
		got[rule.Original] = rule.Renamed
	}
	if !reflect.DeepEqual(got, wantFinalizers) {
		t.Fatalf("finalizer names = %v, want %v", got, wantFinalizers)
	}

	for _, rule := range OperatorNelmRewriteRules.Labels.Names {
		if !strings.HasSuffix(rule.Renamed, internal) {
			t.Fatalf("label %q renamed to %q, outside the internal prefix", rule.Original, rule.Renamed)
		}
		if !strings.HasSuffix(rule.Original, "toolkit.fluxcd.io") {
			t.Fatalf("label rule still matches the fork: %q", rule.Original)
		}
	}
}

// TestNoShortNamesAndNoCategory pins that the internal kinds claim neither. The
// upstream short names would take "hr" and "hc" from a real flux in the cluster,
// and the upstream categories include "all".
func TestNoShortNamesAndNoCategory(t *testing.T) {
	if OperatorNelmRewriteRules.ShortNamePrefix != "" {
		t.Fatalf("short name prefix is %q, want none", OperatorNelmRewriteRules.ShortNamePrefix)
	}
	if len(OperatorNelmRewriteRules.Categories) != 0 {
		t.Fatalf("categories = %v, want none", OperatorNelmRewriteRules.Categories)
	}

	for group, rules := range OperatorNelmAPIGroupsRules {
		for name, rule := range rules.ResourceRules {
			if len(rule.ShortNames) != 0 {
				t.Fatalf("%s/%s declares short names %v", group, name, rule.ShortNames)
			}
			if len(rule.Categories) != 0 {
				t.Fatalf("%s/%s declares categories %v", group, name, rule.Categories)
			}
		}
	}
}
