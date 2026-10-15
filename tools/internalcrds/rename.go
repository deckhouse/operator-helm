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

package main

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// internalPrefix is the group suffix upstream flux groups map onto, shared with the api rewriter.
const internalPrefix = "internal.operator-helm.deckhouse.io"

// internalGroup maps an upstream flux API group to the internal one this module serves.
// It returns an empty string for anything but the two flux groups.
func internalGroup(group string) string {
	switch group {
	case "source.toolkit.fluxcd.io":
		return "source." + internalPrefix
	case "helm.toolkit.fluxcd.io":
		return "helm." + internalPrefix
	default:
		return ""
	}
}

// upstreamKinds lists every kind the two upstream flux controllers declare.
var upstreamKinds = []string{
	"Bucket",
	"ExternalArtifact",
	"GitRepository",
	"HelmChart",
	"HelmRelease",
	"HelmRepository",
	"OCIRepository",
}

// kindPrefix is prepended to every upstream kind; it names live objects, so it cannot change.
const kindPrefix = "InternalNelmOperator"

// kindWord matches an upstream kind standing alone as a word, not one inside a longer name.
var kindWord = regexp.MustCompile(`\b(` + strings.Join(upstreamKinds, "|") + `)\b`)

// rename turns one upstream CustomResourceDefinition into the internal one this module serves.
// It rewrites the group, the names block, the object name and every kind reference in the schemas.
func rename(doc map[string]any) error {
	spec, ok := doc["spec"].(map[string]any)
	if !ok {
		return errors.New("the document has no spec")
	}

	group, _ := spec["group"].(string)
	internal := internalGroup(group)
	if internal == "" {
		return fmt.Errorf("unexpected api group %q", group)
	}
	spec["group"] = internal

	names, ok := spec["names"].(map[string]any)
	if !ok {
		return errors.New("the document has no spec.names")
	}

	kind, ok := names["kind"].(string)
	if !ok {
		return errors.New("the document has no spec.names.kind")
	}
	names["kind"] = kindPrefix + kind
	if listKind, ok := names["listKind"].(string); ok {
		names["listKind"] = kindPrefix + listKind
	}

	plural, ok := names["plural"].(string)
	if !ok {
		return errors.New("the document has no spec.names.plural")
	}
	names["plural"] = strings.ToLower(kindPrefix) + plural
	if singular, ok := names["singular"].(string); ok {
		names["singular"] = strings.ToLower(kindPrefix) + singular
	}

	// Dropping these leaves "hr", "hc" and the "all" category to a real flux in the cluster.
	delete(names, "shortNames")
	delete(names, "categories")

	metadata, ok := doc["metadata"].(map[string]any)
	if !ok {
		return errors.New("the document has no metadata")
	}
	if annotations, ok := metadata["annotations"].(map[string]any); ok {
		delete(annotations, "kustomize.toolkit.fluxcd.io/substitute")

		if len(annotations) == 0 {
			delete(metadata, "annotations")
		}
	}

	metadata["name"] = names["plural"].(string) + "." + internal
	metadata["labels"] = map[string]any{
		"backup.deckhouse.io/cluster-config": "true",
		"heritage":                           "deckhouse",
		"module":                             "operator-helm",
	}

	renameKindReferences(spec)

	return nil
}

// renameKindReferences renames upstream kinds under a "kind" key, in string lists and in free text.
func renameKindReferences(node any) {
	switch typed := node.(type) {
	case map[string]any:
		for key, value := range typed {
			str, ok := value.(string)
			if !ok {
				renameKindReferences(value)

				continue
			}

			switch {
			case key == "kind" && slices.Contains(upstreamKinds, str):
				typed[key] = kindPrefix + str
			case key == "description", key == "message", key == "rule":
				typed[key] = kindWord.ReplaceAllString(str, kindPrefix+"${1}")
			}
		}
	case []any:
		for i, value := range typed {
			if str, ok := value.(string); ok && slices.Contains(upstreamKinds, str) {
				typed[i] = kindPrefix + str

				continue
			}

			renameKindReferences(value)
		}
	}
}
