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

import "testing"

func TestCheckNoLeftoverUpstreamFailsOnLeftoverGroup(t *testing.T) {
	rendered := "spec:\n  group: source.toolkit.fluxcd.io\n"

	err := checkNoLeftoverUpstream("out.yaml", []byte(rendered))
	if err == nil {
		t.Fatal("expected an error, got nil")
	}

	const want = `out.yaml:2: leftover upstream identity: group: source.toolkit.fluxcd.io`
	if err.Error() != want {
		t.Fatalf("error = %q, want %q", err.Error(), want)
	}
}

func TestCheckNoLeftoverUpstreamAllowsSubstituteAnnotation(t *testing.T) {
	rendered := "metadata:\n  annotations:\n    kustomize.toolkit.fluxcd.io/substitute: \"true\"\n"

	if err := checkNoLeftoverUpstream("out.yaml", []byte(rendered)); err != nil {
		t.Fatalf("checkNoLeftoverUpstream returned %v", err)
	}
}
