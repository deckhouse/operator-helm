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

// Command crddoc finishes the CustomResourceDefinitions controller-gen renders
// for the documentation site. Two things the site needs cannot be expressed as
// a kubebuilder marker: apiVersion, kind and metadata have to carry x-doc-skip,
// and the fields of metav1.Condition have to carry the module's own descriptions
// instead of the apimachinery ones. The tool rewrites each file in place through
// the same JSON-then-yaml.v2 path controller-gen renders with, so nothing but
// those additions changes.
//
// Usage:
//
//	crddoc <file>...
package main

import (
	"flag"
	"fmt"
	"os"

	"sigs.k8s.io/yaml"
)

// docSkipped are the root properties every kind shares with the rest of the API
// and the documentation site must not list again for each of them.
var docSkipped = []string{"apiVersion", "kind", "metadata"}

// conditionDescriptions replace the ones apimachinery declares on metav1.Condition.
var conditionDescriptions = map[string]string{
	"lastTransitionTime": "Time when the condition status last changed.",
	"message":            "Message with additional information about the condition state.",
	"observedGeneration": "Resource generation on which the condition state is based.",
	"reason":             "Reason for the last change in the condition state.",
	"status":             "Condition status.",
	"type":               "Condition type.",
}

func main() {
	flag.Parse()

	if flag.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "usage: crddoc <file>...")
		os.Exit(2)
	}

	if err := run(flag.Args()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(paths []string) error {
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}

		doc := map[string]any{}
		if err := yaml.Unmarshal(raw, &doc); err != nil {
			return fmt.Errorf("parsing %s: %w", path, err)
		}

		if err := finish(doc); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}

		encoded, err := yaml.Marshal(doc)
		if err != nil {
			return fmt.Errorf("encoding %s: %w", path, err)
		}

		if err := os.WriteFile(path, append([]byte("---\n"), encoded...), 0o644); err != nil {
			return err
		}
	}

	return nil
}

func finish(doc map[string]any) error {
	spec, _ := doc["spec"].(map[string]any)
	versions, _ := spec["versions"].([]any)

	if len(versions) == 0 {
		return fmt.Errorf("the definition declares no versions")
	}

	for _, item := range versions {
		version, _ := item.(map[string]any)
		schema, _ := version["schema"].(map[string]any)
		root, _ := schema["openAPIV3Schema"].(map[string]any)
		properties, _ := root["properties"].(map[string]any)

		for _, name := range docSkipped {
			property, ok := properties[name].(map[string]any)
			if !ok {
				return fmt.Errorf("version %v declares no %s property", version["name"], name)
			}

			property["x-doc-skip"] = true
		}

		if describeConditions(root) == 0 {
			return fmt.Errorf("version %v declares no conditions", version["name"])
		}
	}

	return nil
}

// describeConditions walks the schema, rewrites the field descriptions of every
// object shaped like metav1.Condition and reports how many it found. The shape,
// not the apimachinery description, identifies it: a rewording upstream must not
// silently bring the upstream text back. A field added upstream changes the
// shape instead, which the caller turns into a failure rather than a silent
// return to the upstream text.
func describeConditions(schema map[string]any) int {
	properties, _ := schema["properties"].(map[string]any)

	if isCondition(properties) {
		for name, description := range conditionDescriptions {
			property, _ := properties[name].(map[string]any)
			property["description"] = description
		}

		return 1
	}

	found := 0

	for _, property := range properties {
		if nested, ok := property.(map[string]any); ok {
			found += describeConditions(nested)
		}
	}

	if items, ok := schema["items"].(map[string]any); ok {
		found += describeConditions(items)
	}

	return found
}

func isCondition(properties map[string]any) bool {
	if len(properties) != len(conditionDescriptions) {
		return false
	}

	for name := range conditionDescriptions {
		if _, ok := properties[name].(map[string]any); !ok {
			return false
		}
	}

	return true
}
