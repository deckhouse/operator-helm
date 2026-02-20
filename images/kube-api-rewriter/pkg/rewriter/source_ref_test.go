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

package rewriter

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// testRulesWithKindRefPaths builds rules with custom kind names to prove
// data-driven behavior. Uses "SomeResource" and "OtherResource" (NOT
// "HelmChart"/"HelmRelease") so the hardcoded switch will NOT match.
func testRulesWithKindRefPaths() *RewriteRules {
	rules := &RewriteRules{
		KindPrefix:         "Prefixed",
		ResourceTypePrefix: "prefixed",
		ShortNamePrefix:    "p",
		Rules: map[string]APIGroupRule{
			"original.group.io": {
				GroupRule: GroupRule{
					Group:            "original.group.io",
					Versions:         []string{"v1"},
					PreferredVersion: "v1",
					Renamed:          "prefixed.resources.group.io",
				},
				ResourceRules: map[string]ResourceRule{
					"someresources": {
						Kind:             "SomeResource",
						ListKind:         "SomeResourceList",
						Plural:           "someresources",
						Singular:         "someresource",
						Versions:         []string{"v1"},
						PreferredVersion: "v1",
					},
					"otherresources": {
						Kind:             "OtherResource",
						ListKind:         "OtherResourceList",
						Plural:           "otherresources",
						Singular:         "otherresource",
						Versions:         []string{"v1"},
						PreferredVersion: "v1",
					},
				},
			},
		},
		KindRefPaths: map[string][]string{
			"SomeResource":  {"spec.sourceRef"},
			"OtherResource": {"spec.chart.spec.sourceRef", "spec.chartRef"},
		},
	}
	rules.Init()
	return rules
}

// TestRewriteSpecKindRefs_RestoreKnownKind tests that Restore rewrites a renamed
// kind back to its original in spec.sourceRef for SomeResource.
func TestRewriteSpecKindRefs_RestoreKnownKind(t *testing.T) {
	rules := testRulesWithKindRefPaths()

	// SomeResource has been renamed to PrefixedSomeResource. Its sourceRef
	// contains a renamed kind that should be restored.
	obj := []byte(`{"kind":"PrefixedSomeResource","spec":{"sourceRef":{"kind":"PrefixedSomeResource"}}}`)

	result, err := RewriteSpecKindRefs(rules, obj, Restore)
	require.NoError(t, err)

	got := gjson.GetBytes(result, "spec.sourceRef.kind").String()
	require.Equal(t, "SomeResource", got, "sourceRef.kind should be restored to original")
}

// TestRewriteSpecKindRefs_RenameKnownKind tests that Rename rewrites an original
// kind to the prefixed form in spec.sourceRef for SomeResource.
func TestRewriteSpecKindRefs_RenameKnownKind(t *testing.T) {
	rules := testRulesWithKindRefPaths()

	// SomeResource (original kind) with sourceRef referencing another known kind.
	obj := []byte(`{"kind":"SomeResource","spec":{"sourceRef":{"kind":"SomeResource"}}}`)

	result, err := RewriteSpecKindRefs(rules, obj, Rename)
	require.NoError(t, err)

	got := gjson.GetBytes(result, "spec.sourceRef.kind").String()
	require.Equal(t, "PrefixedSomeResource", got, "sourceRef.kind should be renamed with prefix")
}

// TestRewriteSpecKindRefs_RestoreMultiplePaths tests that OtherResource with two
// paths (spec.chart.spec.sourceRef and spec.chartRef) both get rewritten.
func TestRewriteSpecKindRefs_RestoreMultiplePaths(t *testing.T) {
	rules := testRulesWithKindRefPaths()

	obj := []byte(`{
		"kind":"PrefixedOtherResource",
		"spec":{
			"chart":{"spec":{"sourceRef":{"kind":"PrefixedSomeResource"}}},
			"chartRef":{"kind":"PrefixedOtherResource"}
		}
	}`)

	result, err := RewriteSpecKindRefs(rules, obj, Restore)
	require.NoError(t, err)

	sourceRefKind := gjson.GetBytes(result, "spec.chart.spec.sourceRef.kind").String()
	require.Equal(t, "SomeResource", sourceRefKind, "chart.spec.sourceRef.kind should be restored")

	chartRefKind := gjson.GetBytes(result, "spec.chartRef.kind").String()
	require.Equal(t, "OtherResource", chartRefKind, "chartRef.kind should be restored")
}

// TestRewriteSpecKindRefs_UnknownKindPassThrough tests that a kind not in
// KindRefPaths (e.g. ConfigMap) is returned unchanged.
func TestRewriteSpecKindRefs_UnknownKindPassThrough(t *testing.T) {
	rules := testRulesWithKindRefPaths()

	obj := []byte(`{"kind":"ConfigMap","spec":{"sourceRef":{"kind":"SomeResource"}}}`)

	result, err := RewriteSpecKindRefs(rules, obj, Restore)
	require.NoError(t, err)

	// sourceRef should be untouched since ConfigMap is not in KindRefPaths.
	got := gjson.GetBytes(result, "spec.sourceRef.kind").String()
	require.Equal(t, "SomeResource", got, "unknown kind should pass through unchanged")
}

// TestRewriteSpecKindRefs_NilKindRefPaths tests that nil KindRefPaths means
// all objects pass through unchanged.
func TestRewriteSpecKindRefs_NilKindRefPaths(t *testing.T) {
	rules := testRulesWithKindRefPaths()
	rules.KindRefPaths = nil

	obj := []byte(`{"kind":"PrefixedSomeResource","spec":{"sourceRef":{"kind":"PrefixedSomeResource"}}}`)

	result, err := RewriteSpecKindRefs(rules, obj, Restore)
	require.NoError(t, err)

	// Should be unchanged since KindRefPaths is nil.
	got := gjson.GetBytes(result, "spec.sourceRef.kind").String()
	require.Equal(t, "PrefixedSomeResource", got, "nil KindRefPaths should pass through")
}

// TestRewritePatchSourceRefs_RewritesAllPaths tests that patches rewrite kind
// references across all configured paths.
func TestRewritePatchSourceRefs_RewritesAllPaths(t *testing.T) {
	rules := testRulesWithKindRefPaths()

	patch := []byte(`{
		"spec":{
			"sourceRef":{"kind":"SomeResource"},
			"chart":{"spec":{"sourceRef":{"kind":"OtherResource"}}},
			"chartRef":{"kind":"SomeResource"}
		}
	}`)

	result, err := RewritePatchSourceRefs(rules, patch)
	require.NoError(t, err)

	sourceRefKind := gjson.GetBytes(result, "spec.sourceRef.kind").String()
	require.Equal(t, "PrefixedSomeResource", sourceRefKind, "sourceRef.kind should be renamed")

	chartSourceRefKind := gjson.GetBytes(result, "spec.chart.spec.sourceRef.kind").String()
	require.Equal(t, "PrefixedOtherResource", chartSourceRefKind, "chart.spec.sourceRef.kind should be renamed")

	chartRefKind := gjson.GetBytes(result, "spec.chartRef.kind").String()
	require.Equal(t, "PrefixedSomeResource", chartRefKind, "chartRef.kind should be renamed")
}

// TestRewritePatchSourceRefs_NilKindRefPaths tests that nil KindRefPaths means
// patches pass through unchanged.
func TestRewritePatchSourceRefs_NilKindRefPaths(t *testing.T) {
	rules := testRulesWithKindRefPaths()
	rules.KindRefPaths = nil

	patch := []byte(`{"spec":{"sourceRef":{"kind":"SomeResource"}}}`)

	result, err := RewritePatchSourceRefs(rules, patch)
	require.NoError(t, err)

	got := gjson.GetBytes(result, "spec.sourceRef.kind").String()
	require.Equal(t, "SomeResource", got, "nil KindRefPaths should pass through")
}

// TestRewritePatchSourceRefs_EmptyPatch tests that empty input returns empty.
func TestRewritePatchSourceRefs_EmptyPatch(t *testing.T) {
	rules := testRulesWithKindRefPaths()

	result, err := RewritePatchSourceRefs(rules, []byte{})
	require.NoError(t, err)
	require.Empty(t, result)
}

// TestRewritePatchSourceRefs_ArrayPatch tests that JSON array patches pass through.
func TestRewritePatchSourceRefs_ArrayPatch(t *testing.T) {
	rules := testRulesWithKindRefPaths()

	patch := []byte(`[{"op":"replace","path":"/spec/sourceRef/kind","value":"SomeResource"}]`)

	result, err := RewritePatchSourceRefs(rules, patch)
	require.NoError(t, err)

	// Array patches should pass through unchanged (they start with '[' not '{').
	require.Equal(t, string(patch), string(result))
}
