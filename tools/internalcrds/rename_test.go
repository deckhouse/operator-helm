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

func TestRenameFailsOnMissingNames(t *testing.T) {
	cases := map[string]struct {
		names map[string]any
		want  string
	}{
		"kind missing":      {map[string]any{"plural": "helmreleases", "singular": "helmrelease"}, "the document has no spec.names.kind"},
		"kind not a string": {map[string]any{"kind": 1, "plural": "helmreleases", "singular": "helmrelease"}, "the document has no spec.names.kind"},
		"plural missing":    {map[string]any{"kind": "HelmRelease", "singular": "helmrelease"}, "the document has no spec.names.plural"},
	}

	for name, tc := range cases {
		doc := map[string]any{
			"spec": map[string]any{
				"group": "helm.toolkit.fluxcd.io",
				"names": tc.names,
			},
			"metadata": map[string]any{"name": "helmreleases.helm.toolkit.fluxcd.io"},
		}

		err := Rename(doc)
		if err == nil {
			t.Fatalf("%s: expected an error, got nil", name)
		}

		if err.Error() != tc.want {
			t.Fatalf("%s: error = %q, want %q", name, err.Error(), tc.want)
		}
	}
}

func TestRenameRewritesKindNamesInProse(t *testing.T) {
	doc := map[string]any{
		"spec": map[string]any{
			"group": "source.toolkit.fluxcd.io",
			"names": map[string]any{"kind": "HelmChart", "plural": "helmcharts", "singular": "helmchart"},
			"versions": []any{map[string]any{
				"schema": map[string]any{"openAPIV3Schema": map[string]any{
					"description": "HelmChart is the Schema for the helmcharts API.",
					"properties": map[string]any{"spec": map[string]any{
						"properties": map[string]any{"sourceRef": map[string]any{
							"properties": map[string]any{"kind": map[string]any{
								"description": "Kind of the referent, valid values are ('HelmRepository', 'GitRepository', 'Bucket').",
							}},
						}},
						"x-kubernetes-validations": []any{map[string]any{
							"message": "spec.verify is only supported when spec.sourceRef.kind is 'HelmRepository'",
							"rule":    "!has(self.verify) || self.sourceRef.kind == 'HelmRepository'",
						}},
					}},
					"status": map[string]any{
						"description": "HelmChartStatus records the observed state of the HelmChart.",
					},
				}},
			}},
		},
		"metadata": map[string]any{"name": "helmcharts.source.toolkit.fluxcd.io"},
	}

	if err := Rename(doc); err != nil {
		t.Fatalf("Rename returned %v", err)
	}

	schema := doc["spec"].(map[string]any)["versions"].([]any)[0].(map[string]any)["schema"].(map[string]any)["openAPIV3Schema"].(map[string]any)
	specSchema := schema["properties"].(map[string]any)["spec"].(map[string]any)
	validation := specSchema["x-kubernetes-validations"].([]any)[0].(map[string]any)

	cases := map[string]struct {
		got  any
		want string
	}{
		"schema description": {
			schema["description"],
			"InternalNelmOperatorHelmChart is the Schema for the helmcharts API.",
		},
		"referent description": {
			specSchema["properties"].(map[string]any)["sourceRef"].(map[string]any)["properties"].(map[string]any)["kind"].(map[string]any)["description"],
			"Kind of the referent, valid values are ('InternalNelmOperatorHelmRepository', 'InternalNelmOperatorGitRepository', 'InternalNelmOperatorBucket').",
		},
		"validation message": {
			validation["message"],
			"spec.verify is only supported when spec.sourceRef.kind is 'InternalNelmOperatorHelmRepository'",
		},
		"validation rule": {
			validation["rule"],
			"!has(self.verify) || self.sourceRef.kind == 'InternalNelmOperatorHelmRepository'",
		},
		// A composite upstream type name is not a kind and stays as upstream
		// wrote it; only the kind it is named after moves.
		"composite type name": {
			schema["status"].(map[string]any)["description"],
			"HelmChartStatus records the observed state of the InternalNelmOperatorHelmChart.",
		},
	}

	for name, tc := range cases {
		if tc.got != tc.want {
			t.Errorf("%s = %v, want %q", name, tc.got, tc.want)
		}
	}
}

func TestRenameDropsTheSubstituteAnnotation(t *testing.T) {
	doc := map[string]any{
		"spec": map[string]any{
			"group": "helm.toolkit.fluxcd.io",
			"names": map[string]any{"kind": "HelmRelease", "plural": "helmreleases", "singular": "helmrelease"},
		},
		"metadata": map[string]any{
			"name": "helmreleases.helm.toolkit.fluxcd.io",
			"annotations": map[string]any{
				"controller-gen.kubebuilder.io/version":  "v0.21.0",
				"kustomize.toolkit.fluxcd.io/substitute": "disabled",
			},
		},
	}

	if err := Rename(doc); err != nil {
		t.Fatalf("Rename returned %v", err)
	}

	annotations := doc["metadata"].(map[string]any)["annotations"].(map[string]any)
	if _, ok := annotations["kustomize.toolkit.fluxcd.io/substitute"]; ok {
		t.Error("the substitute annotation survived")
	}

	if annotations["controller-gen.kubebuilder.io/version"] != "v0.21.0" {
		t.Errorf("unrelated annotations = %v", annotations)
	}
}

func TestRenameDropsAnEmptiedAnnotationsBlock(t *testing.T) {
	doc := map[string]any{
		"spec": map[string]any{
			"group": "helm.toolkit.fluxcd.io",
			"names": map[string]any{"kind": "HelmRelease", "plural": "helmreleases", "singular": "helmrelease"},
		},
		"metadata": map[string]any{
			"name":        "helmreleases.helm.toolkit.fluxcd.io",
			"annotations": map[string]any{"kustomize.toolkit.fluxcd.io/substitute": "disabled"},
		},
	}

	if err := Rename(doc); err != nil {
		t.Fatalf("Rename returned %v", err)
	}

	if _, ok := doc["metadata"].(map[string]any)["annotations"]; ok {
		t.Error("an empty annotations block was left behind")
	}
}
