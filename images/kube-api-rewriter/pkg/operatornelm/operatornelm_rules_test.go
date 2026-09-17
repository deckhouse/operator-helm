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
