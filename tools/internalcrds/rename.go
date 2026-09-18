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

// internalPrefix is where every upstream flux group is mapped to. It matches the
// prefix the api rewriter uses, and the two must never diverge: a resource the
// proxy renames to a group no CustomResourceDefinition declares is simply not
// found, and that only shows up against a live cluster.
const internalPrefix = "internal.operator-helm.deckhouse.io"

// InternalGroup maps an upstream flux API group to the internal one this module
// serves. Anything but the two flux groups is a programming error: the generator
// is fed a fixed list of upstream CustomResourceDefinitions.
func InternalGroup(group string) string {
	switch group {
	case "source.toolkit.fluxcd.io":
		return "source." + internalPrefix
	case "helm.toolkit.fluxcd.io":
		return "helm." + internalPrefix
	default:
		return ""
	}
}

// upstreamKinds lists every kind the two upstream controllers declare, so a
// reference inside a schema can be recognised and renamed. A kind missing here
// silently stays unrenamed, which is why Rename fails on an unknown group
// rather than guessing.
var upstreamKinds = []string{
	"Bucket",
	"ExternalArtifact",
	"GitRepository",
	"HelmChart",
	"HelmRelease",
	"HelmRepository",
	"OCIRepository",
}

// kindPrefix is prepended to every upstream kind. It carries the word "Nelm"
// for historical reasons: these are the names of live objects in every cluster
// running the module, and moving them would be a migration of its own.
const kindPrefix = "InternalNelmOperator"

// kindWord matches an upstream kind standing on its own as a word, which is
// how the schemas refer to one in prose and in validation expressions. The
// word boundaries keep it off the type names built from a kind — a
// HelmChartStatus is upstream's own type and is not served under any name
// here — and off a renamed kind, whose prefix leaves no boundary in front.
var kindWord = regexp.MustCompile(`\b(` + strings.Join(upstreamKinds, "|") + `)\b`)

// Rename turns one upstream CustomResourceDefinition into the internal one this
// module serves: the group, the names block, the object name, and every
// reference to a kind inside the schemas, whether it is a value the API server
// reads or text a reader does.
func Rename(doc map[string]any) error {
	spec, ok := doc["spec"].(map[string]any)
	if !ok {
		return errors.New("the document has no spec")
	}

	group, _ := spec["group"].(string)
	internal := InternalGroup(group)
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

	// Service objects: nobody types them, and the upstream values would take
	// "hr", "hc" and the "all" category away from a real flux in the cluster.
	delete(names, "shortNames")
	delete(names, "categories")

	metadata, ok := doc["metadata"].(map[string]any)
	if !ok {
		return errors.New("the document has no metadata")
	}
	// Upstream tells a real flux kustomize-controller not to substitute
	// variables into these definitions. Nothing applies them through one —
	// Deckhouse installs them with the module — so the instruction has no
	// reader here and only leaves an upstream identity on the object.
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

// renameKindReferences walks the schemas and renames every upstream kind it
// finds: under a "kind" key in a map; with no key to scope the match, any
// plain string list element equal to a kind; and inside the three free-text
// fields that name kinds — the documentation a reader gets from kubectl
// explain, and the message and expression of a validation rule. Today the
// only lists it reaches are the sourceRef and chartRef kind enums, so
// matching a list element by value alone is safe; a future field whose string
// entries happened to equal an upstream kind's name would be renamed too.
//
// Renaming the expression of a rule is not cosmetic: a rule comparing against
// an upstream kind can never hold once the enum beside it is renamed.
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
