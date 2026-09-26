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

package operatornelm

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
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

// property is the part of an OpenAPI schema a kind reference path walks
// through. The proxy addresses these paths with gjson on a plain object
// (see rewriter.TransformObject), so nothing but a named field of an object
// is ever traversed: no array items, no additionalProperties.
type property struct {
	Properties map[string]property `json:"properties"`
}

type definition struct {
	// file records where the definition was read from, so a failure names a
	// file to open rather than a CRD name to grep for.
	file string

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
			Schema  struct {
				OpenAPIV3Schema property `json:"openAPIV3Schema"`
			} `json:"schema"`
		} `json:"versions"`
	} `json:"spec"`
}

// loadDefinitions reads every generated internal definition, keyed by the
// name the API server knows, which is the one thing both sides derive
// independently.
func loadDefinitions(t *testing.T) map[string]definition {
	t.Helper()

	paths, err := filepath.Glob(definitionsGlob)
	if err != nil || len(paths) == 0 {
		t.Fatalf("no definitions found at %s: %v", definitionsGlob, err)
	}

	separator := regexp.MustCompile(`(?m)^---$`)

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

			parsed.file = path
			definitions[parsed.Metadata.Name] = parsed
		}
	}

	return definitions
}

func TestRulesMatchGeneratedDefinitions(t *testing.T) {
	definitions := loadDefinitions(t)

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

// TestKindRefPathsExistInGeneratedDefinitions keeps the kind reference paths
// honest. A path the schema does not declare is silently skipped by the
// proxy: the reference keeps the upstream kind, and the object reaches the
// cluster pointing at a kind that does not exist there. A typo, or a field
// upstream moved during a flux bump, looks exactly like a working rule
// until then.
func TestKindRefPathsExistInGeneratedDefinitions(t *testing.T) {
	definitions := loadDefinitions(t)

	byKind := make(map[string]definition, len(definitions))
	for _, parsed := range definitions {
		byKind[parsed.Spec.Names.Kind] = parsed
	}

	kinds := make([]string, 0, len(OperatorNelmRewriteRules.KindRefPaths))
	for kind := range OperatorNelmRewriteRules.KindRefPaths {
		kinds = append(kinds, kind)
	}

	sort.Strings(kinds)

	for _, kind := range kinds {
		renamed := OperatorNelmRewriteRules.KindPrefix + kind

		parsed, ok := byKind[renamed]
		if !ok {
			t.Errorf("kind reference paths of %s: no definition declares %s", kind, renamed)

			continue
		}

		for _, path := range OperatorNelmRewriteRules.KindRefPaths[kind] {
			for _, version := range parsed.Spec.Versions {
				if !version.Served {
					continue
				}

				ref, parent, missing := resolvePath(version.Schema.OpenAPIV3Schema, path)
				if missing != "" {
					t.Errorf("%s: kind reference path %q of %s: %s %s declares no %q under %s (%s)",
						parsed.Metadata.Name, path, kind, renamed, version.Name,
						missing, parent, parsed.file)

					continue
				}

				// Reaching a field that holds no kind means the path lost
				// its meaning even though every segment still resolves.
				if _, ok := ref.Properties["kind"]; !ok {
					t.Errorf("%s: kind reference path %q of %s: %s %s declares it with no kind field (%s)",
						parsed.Metadata.Name, path, kind, renamed, version.Name,
						parsed.file)
				}
			}
		}
	}
}

// resolvePath walks a dotted kind reference path through the schema the same
// way the proxy walks it through an object: every segment is a named field
// of the object before it. It returns the schema the path ends at, or the
// segment the schema stops declaring together with the part of the path
// that should have declared it.
func resolvePath(schema property, path string) (ref property, parent, missing string) {
	current := schema
	segments := strings.Split(path, ".")

	for i, segment := range segments {
		next, ok := current.Properties[segment]
		if !ok {
			parent = strings.Join(segments[:i], ".")
			if parent == "" {
				parent = "the root"
			}

			return property{}, parent, segment
		}

		current = next
	}

	return current, "", ""
}
