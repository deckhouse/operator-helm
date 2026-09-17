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
	"reflect"
	"testing"
)

func TestInternalGroup(t *testing.T) {
	cases := map[string]string{
		"source.toolkit.fluxcd.io": "source.internal.operator-helm.deckhouse.io",
		"helm.toolkit.fluxcd.io":   "helm.internal.operator-helm.deckhouse.io",
	}

	for in, want := range cases {
		if got := InternalGroup(in); got != want {
			t.Fatalf("InternalGroup(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRenameRewritesEveryIdentity(t *testing.T) {
	doc := map[string]any{
		"apiVersion": "apiextensions.k8s.io/v1",
		"kind":       "CustomResourceDefinition",
		"metadata": map[string]any{
			"name": "helmreleases.helm.toolkit.fluxcd.io",
		},
		"spec": map[string]any{
			"group": "helm.toolkit.fluxcd.io",
			"names": map[string]any{
				"kind":       "HelmRelease",
				"listKind":   "HelmReleaseList",
				"plural":     "helmreleases",
				"singular":   "helmrelease",
				"shortNames": []any{"hr"},
				"categories": []any{"all", "fluxcd"},
			},
		},
	}

	if err := Rename(doc); err != nil {
		t.Fatalf("Rename returned %v", err)
	}

	spec := doc["spec"].(map[string]any)
	names := spec["names"].(map[string]any)
	meta := doc["metadata"].(map[string]any)

	if got := spec["group"]; got != "helm.internal.operator-helm.deckhouse.io" {
		t.Fatalf("group = %v", got)
	}
	if got := names["kind"]; got != "InternalNelmOperatorHelmRelease" {
		t.Fatalf("kind = %v", got)
	}
	if got := names["listKind"]; got != "InternalNelmOperatorHelmReleaseList" {
		t.Fatalf("listKind = %v", got)
	}
	if got := names["plural"]; got != "internalnelmoperatorhelmreleases" {
		t.Fatalf("plural = %v", got)
	}
	if got := names["singular"]; got != "internalnelmoperatorhelmrelease" {
		t.Fatalf("singular = %v", got)
	}
	if got := meta["name"]; got != "internalnelmoperatorhelmreleases.helm.internal.operator-helm.deckhouse.io" {
		t.Fatalf("metadata.name = %v", got)
	}

	// Short names and categories are dropped rather than renamed: these are
	// service objects, and the upstream values would collide with a real flux.
	if _, ok := names["shortNames"]; ok {
		t.Fatal("shortNames must be gone")
	}
	if _, ok := names["categories"]; ok {
		t.Fatal("categories must be gone")
	}
}

func TestRenameRewritesNestedReferences(t *testing.T) {
	doc := map[string]any{
		"spec": map[string]any{
			"group": "helm.toolkit.fluxcd.io",
			"names": map[string]any{"kind": "HelmRelease", "plural": "helmreleases", "singular": "helmrelease"},
			"versions": []any{map[string]any{
				"schema": map[string]any{"openAPIV3Schema": map[string]any{
					"properties": map[string]any{"spec": map[string]any{
						"properties": map[string]any{"chartRef": map[string]any{
							"properties": map[string]any{
								"kind": map[string]any{
									"enum": []any{"OCIRepository", "HelmChart"},
								},
							},
						}},
					}},
				}},
			}},
		},
		"metadata": map[string]any{"name": "helmreleases.helm.toolkit.fluxcd.io"},
	}

	if err := Rename(doc); err != nil {
		t.Fatalf("Rename returned %v", err)
	}

	enum := doc["spec"].(map[string]any)["versions"].([]any)[0].(map[string]any)["schema"].(map[string]any)["openAPIV3Schema"].(map[string]any)["properties"].(map[string]any)["spec"].(map[string]any)["properties"].(map[string]any)["chartRef"].(map[string]any)["properties"].(map[string]any)["kind"].(map[string]any)["enum"].([]any)

	want := []any{"InternalNelmOperatorOCIRepository", "InternalNelmOperatorHelmChart"}
	if !reflect.DeepEqual(enum, want) {
		t.Fatalf("chartRef kind enum = %v, want %v", enum, want)
	}
}

func TestRenameLeavesUnrelatedStringsAlone(t *testing.T) {
	doc := map[string]any{
		"spec": map[string]any{
			"group": "source.toolkit.fluxcd.io",
			"names": map[string]any{"kind": "HelmChart", "plural": "helmcharts", "singular": "helmchart"},
			"versions": []any{map[string]any{
				"schema": map[string]any{"openAPIV3Schema": map[string]any{
					"description": "HelmChart is the Schema for the helmcharts API.",
				}},
			}},
		},
		"metadata": map[string]any{"name": "helmcharts.source.toolkit.fluxcd.io"},
	}

	if err := Rename(doc); err != nil {
		t.Fatalf("Rename returned %v", err)
	}

	got := doc["spec"].(map[string]any)["versions"].([]any)[0].(map[string]any)["schema"].(map[string]any)["openAPIV3Schema"].(map[string]any)["description"]
	if got != "HelmChart is the Schema for the helmcharts API." {
		t.Fatalf("description was rewritten: %v", got)
	}
}
