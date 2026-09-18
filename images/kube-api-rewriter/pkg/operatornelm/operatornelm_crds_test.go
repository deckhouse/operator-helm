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
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// definitionsGlob finds the definitions the generator writes from upstream
// flux. They are the other half of the rules in this package: the proxy
// renames a request to a group, kind and resource type, and one of these
// declares them. A resource renamed to something no definition declares is
// not found, and nothing but a live cluster says so.
const definitionsGlob = "../../../../crds/embedded/*.yaml"

type definition struct {
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Spec struct {
		Group string `json:"group"`
		Names struct {
			Kind       string   `json:"kind"`
			ListKind   string   `json:"listKind"`
			Plural     string   `json:"plural"`
			Singular   string   `json:"singular"`
			ShortNames []string `json:"shortNames"`
			Categories []string `json:"categories"`
		} `json:"names"`
		Versions []struct {
			Name    string `json:"name"`
			Served  bool   `json:"served"`
			Storage bool   `json:"storage"`
		} `json:"versions"`
	} `json:"spec"`
}

func TestRulesMatchGeneratedDefinitions(t *testing.T) {
	paths, err := filepath.Glob(definitionsGlob)
	if err != nil || len(paths) == 0 {
		t.Fatalf("no definitions found at %s: %v", definitionsGlob, err)
	}

	separator := regexp.MustCompile(`(?m)^---$`)

	// Keyed by the name the API server knows, which is the one thing both
	// sides derive independently.
	definitions := make(map[string]definition)

	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}

		for _, doc := range separator.Split(string(raw), -1) {
			if strings.TrimSpace(doc) == "" {
				continue
			}

			var parsed definition
			if err := yaml.Unmarshal([]byte(doc), &parsed); err != nil {
				t.Fatalf("%s: %v", path, err)
			}

			definitions[parsed.Metadata.Name] = parsed
		}
	}

	matched := make(map[string]bool, len(definitions))

	for group, groupRule := range OperatorNelmAPIGroupsRules {
		for resourceType, rule := range groupRule.ResourceRules {
			name := OperatorNelmRewriteRules.ResourceTypePrefix + resourceType + "." + groupRule.GroupRule.Renamed

			parsed, ok := definitions[name]
			if !ok {
				t.Errorf("%s/%s is renamed to %s, which no definition declares", group, resourceType, name)

				continue
			}

			matched[name] = true

			checks := map[string][2]any{
				"group":       {parsed.Spec.Group, groupRule.GroupRule.Renamed},
				"kind":        {parsed.Spec.Names.Kind, OperatorNelmRewriteRules.KindPrefix + rule.Kind},
				"list kind":   {parsed.Spec.Names.ListKind, OperatorNelmRewriteRules.KindPrefix + rule.ListKind},
				"plural":      {parsed.Spec.Names.Plural, OperatorNelmRewriteRules.ResourceTypePrefix + rule.Plural},
				"singular":    {parsed.Spec.Names.Singular, OperatorNelmRewriteRules.ResourceTypePrefix + rule.Singular},
				"short names": {len(parsed.Spec.Names.ShortNames), 0},
				"categories":  {len(parsed.Spec.Names.Categories), 0},
			}

			for what, pair := range checks {
				if pair[0] != pair[1] {
					t.Errorf("%s: %s is %v, the rule says %v", name, what, pair[0], pair[1])
				}
			}

			var served []string

			storage := ""

			for _, version := range parsed.Spec.Versions {
				if version.Served {
					served = append(served, version.Name)
				}

				if version.Storage {
					storage = version.Name
				}
			}

			if !reflect.DeepEqual(served, rule.Versions) {
				t.Errorf("%s: serves %v, the rule offers %v", name, served, rule.Versions)
			}

			// The proxy answers discovery with the preferred version, so a
			// definition storing a different one would have every write land
			// in a conversion the module never asked for.
			if storage != rule.PreferredVersion {
				t.Errorf("%s: stores %q, the rule prefers %q", name, storage, rule.PreferredVersion)
			}
		}
	}

	for name := range definitions {
		if !matched[name] {
			t.Errorf("%s is declared but no rule renames anything to it", name)
		}
	}
}
