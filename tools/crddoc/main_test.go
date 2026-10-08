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
	"os"
	"path/filepath"
	"testing"
)

// The input is spelled the way controller-gen renders it, and the expected output
// must keep that spelling: the tool rewrites the committed definitions in place,
// so any formatting of its own would show up as an unrelated diff.
const generated = `---
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: helmapplications.helm.deckhouse.io
spec:
  group: helm.deckhouse.io
  versions:
  - name: v1alpha1
    schema:
      openAPIV3Schema:
        description: HelmApplication describes a Helm release within a single namespace.
        properties:
          apiVersion:
            description: APIVersion defines the versioned schema of this representation
              of an object.
            type: string
          kind:
            description: |-
              Kind is a string value representing the REST resource this object represents.
              In CamelCase.
            type: string
          metadata:
            type: object
          status:
            properties:
              conditions:
                description: Conditions reflecting the current state of the resource.
                items:
                  description: Condition contains details for one aspect of the current
                    state of this API Resource.
                  properties:
                    lastTransitionTime:
                      description: |-
                        lastTransitionTime is the last time the condition transitioned from one status to another.
                        This should be when the underlying condition changed.  If that is not known, then using the time when the API field changed is acceptable.
                      format: date-time
                      type: string
                    message:
                      description: |-
                        message is a human readable message indicating details about the transition.
                        This may be an empty string.
                      maxLength: 32768
                      type: string
                    observedGeneration:
                      description: observedGeneration represents the .metadata.generation
                        that the condition was set based upon.
                      format: int64
                      minimum: 0
                      type: integer
                    reason:
                      description: reason contains a programmatic identifier indicating
                        the reason for the condition's last transition.
                      maxLength: 1024
                      minLength: 1
                      pattern: ^[A-Za-z]([A-Za-z0-9_,:]*[A-Za-z0-9_])?$
                      type: string
                    status:
                      description: status of the condition, one of True, False, Unknown.
                      enum:
                      - "True"
                      - "False"
                      - Unknown
                      type: string
                    type:
                      description: type of condition in CamelCase or in foo.example.com/CamelCase.
                      maxLength: 316
                      type: string
                  required:
                  - lastTransitionTime
                  - message
                  - reason
                  - status
                  - type
                  type: object
                type: array
            type: object
        type: object
    served: true
    storage: true
`

const finished = `---
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: helmapplications.helm.deckhouse.io
spec:
  group: helm.deckhouse.io
  versions:
  - name: v1alpha1
    schema:
      openAPIV3Schema:
        description: HelmApplication describes a Helm release within a single namespace.
        properties:
          apiVersion:
            description: APIVersion defines the versioned schema of this representation
              of an object.
            type: string
            x-doc-skip: true
          kind:
            description: |-
              Kind is a string value representing the REST resource this object represents.
              In CamelCase.
            type: string
            x-doc-skip: true
          metadata:
            type: object
            x-doc-skip: true
          status:
            properties:
              conditions:
                description: Conditions reflecting the current state of the resource.
                items:
                  description: Condition contains details for one aspect of the current
                    state of this API Resource.
                  properties:
                    lastTransitionTime:
                      description: Time when the condition status last changed.
                      format: date-time
                      type: string
                    message:
                      description: Message with additional information about the condition
                        state.
                      maxLength: 32768
                      type: string
                    observedGeneration:
                      description: Resource generation on which the condition state
                        is based.
                      format: int64
                      minimum: 0
                      type: integer
                    reason:
                      description: Reason for the last change in the condition state.
                      maxLength: 1024
                      minLength: 1
                      pattern: ^[A-Za-z]([A-Za-z0-9_,:]*[A-Za-z0-9_])?$
                      type: string
                    status:
                      description: Condition status.
                      enum:
                      - "True"
                      - "False"
                      - Unknown
                      type: string
                    type:
                      description: Condition type.
                      maxLength: 316
                      type: string
                  required:
                  - lastTransitionTime
                  - message
                  - reason
                  - status
                  - type
                  type: object
                type: array
            type: object
        type: object
    served: true
    storage: true
`

func TestFinishMarksTypeMetaAndDescribesConditions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "helmapplications.yaml")
	if err := os.WriteFile(path, []byte(generated), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := run([]string{path}); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	if string(got) != finished {
		t.Fatalf("finished definition differs from the expected one:\n--- got\n%s\n--- want\n%s", got, finished)
	}
}

func TestFinishIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "helmapplications.yaml")
	if err := os.WriteFile(path, []byte(finished), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := run([]string{path}); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	if string(got) != finished {
		t.Fatalf("a second run changed the definition:\n--- got\n%s\n--- want\n%s", got, finished)
	}
}

func TestRunFailsWithoutConditions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "broken.yaml")
	doc := `
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: broken.helm.deckhouse.io
spec:
  versions:
  - name: v1alpha1
    schema:
      openAPIV3Schema:
        properties:
          apiVersion:
            type: string
          kind:
            type: string
          metadata:
            type: object
          status:
            properties:
              conditions:
                items:
                  properties:
                    message:
                      type: string
                    status:
                      type: string
                    type:
                      type: string
                  type: object
                type: array
            type: object
        type: object
`
	if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := run([]string{path}); err == nil {
		t.Fatal("expected an error, got nil")
	}
}

func TestRunFailsWithoutTypeMeta(t *testing.T) {
	path := filepath.Join(t.TempDir(), "broken.yaml")
	doc := `
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: broken.helm.deckhouse.io
spec:
  versions:
  - name: v1alpha1
    schema:
      openAPIV3Schema:
        properties:
          spec:
            type: object
        type: object
`
	if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := run([]string{path}); err == nil {
		t.Fatal("expected an error, got nil")
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	if string(got) != doc {
		t.Fatal("run must leave a definition it cannot finish untouched")
	}
}
