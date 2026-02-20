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
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// RewriteKindRef rewrites the "kind" field in an object that references another
// resource kind (e.g., spec.sourceRef in HelmChart). If "apiVersion" is also
// present, both fields are rewritten using RewriteAPIVersionAndKind.
func RewriteKindRef(rules *RewriteRules, obj []byte, action Action) ([]byte, error) {
	kind := gjson.GetBytes(obj, "kind").String()
	if kind == "" {
		return obj, nil
	}

	apiVersion := gjson.GetBytes(obj, "apiVersion").String()
	if apiVersion != "" {
		return RewriteAPIVersionAndKind(rules, obj, action)
	}

	var rwrKind string
	if action == Rename {
		_, resRule := rules.GroupResourceRulesByKind(kind)
		if resRule == nil {
			return obj, nil
		}
		rwrKind = rules.RenameKind(kind)
	}
	if action == Restore {
		restoredKind := rules.RestoreKind(kind)
		_, resRule := rules.GroupResourceRulesByKind(restoredKind)
		if resRule == nil {
			return obj, nil
		}
		rwrKind = restoredKind
	}

	if rwrKind == "" || rwrKind == kind {
		return obj, nil
	}

	return sjson.SetBytes(obj, "kind", rwrKind)
}

// RewriteSpecKindRefs rewrites kind references in spec fields of known resources.
// It uses KindRefPaths from rules to determine which spec paths contain kind
// references for each resource kind.
func RewriteSpecKindRefs(rules *RewriteRules, obj []byte, action Action) ([]byte, error) {
	kind := gjson.GetBytes(obj, "kind").String()
	origKind := rules.RestoreKind(kind)

	paths := rules.KindRefPathsFor(origKind)
	if len(paths) == 0 {
		return obj, nil
	}

	var err error
	for _, path := range paths {
		obj, err = TransformObject(obj, path, func(refObj []byte) ([]byte, error) {
			return RewriteKindRef(rules, refObj, action)
		})
		if err != nil {
			return nil, err
		}
	}
	return obj, nil
}

// RewritePatchSourceRefs rewrites sourceRef kind references in merge patches.
// It tries all configured KindRefPaths since merge patches do not have a
// top-level kind field to determine the resource type.
func RewritePatchSourceRefs(rules *RewriteRules, patch []byte) ([]byte, error) {
	if len(patch) == 0 || patch[0] != '{' {
		return patch, nil
	}

	paths := rules.AllKindRefPaths()
	if len(paths) == 0 {
		return patch, nil
	}

	var err error
	for _, path := range paths {
		patch, err = TransformObject(patch, path, func(refObj []byte) ([]byte, error) {
			return RewriteKindRef(rules, refObj, Rename)
		})
		if err != nil {
			return nil, err
		}
	}
	return patch, nil
}
