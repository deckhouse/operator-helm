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

// Rename turns one upstream CustomResourceDefinition into the internal one this
// module serves. Everything it touches is addressed by path: the group, the
// names block, the object name, and the kind references inside the schemas.
// Descriptions and any other free text are left exactly as upstream wrote them.
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

	kind, _ := names["kind"].(string)
	names["kind"] = kindPrefix + kind
	if listKind, ok := names["listKind"].(string); ok {
		names["listKind"] = kindPrefix + listKind
	}

	plural, _ := names["plural"].(string)
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
	metadata["name"] = names["plural"].(string) + "." + internal
	metadata["labels"] = map[string]any{
		"backup.deckhouse.io/cluster-config": "true",
		"heritage":                           "deckhouse",
		"module":                             "operator-helm",
	}

	renameKindReferences(spec)

	return nil
}

// renameKindReferences walks the schemas and renames every value that names an
// upstream kind. Those live in sourceRef and chartRef at several depths, as a
// default, as an enum entry, or as an example, so the walk is over the whole
// tree rather than a fixed list of paths.
func renameKindReferences(node any) {
	switch typed := node.(type) {
	case map[string]any:
		for key, value := range typed {
			if str, ok := value.(string); ok && key == "kind" && slices.Contains(upstreamKinds, str) {
				typed[key] = kindPrefix + str

				continue
			}

			renameKindReferences(value)
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
