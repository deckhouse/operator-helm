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

// Command internalcrds turns the upstream flux CustomResourceDefinitions into
// the internal ones this module ships. It reads every yaml document from the
// input directories, renames it, and writes one file per controller.
//
// Usage:
//
//	internalcrds -out crds/embedded/helm-controller.yaml <dir>...
package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
)

// forbiddenGroup must not survive into the rendered output. Its presence means
// some identity slipped past Rename: a kind or a nested group field a future
// flux version introduces in a place the rename table does not yet know about.
const forbiddenGroup = "toolkit.fluxcd.io"

// allowedAnnotation is the sole legitimate survivor of forbiddenGroup: a
// kustomize substitution annotation key upstream declares as an example, not
// an identity this module renames.
const allowedAnnotation = "kustomize.toolkit.fluxcd.io/substitute"

func main() {
	out := flag.String("out", "", "file to write the renamed definitions to")
	flag.Parse()

	if *out == "" || flag.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "usage: internalcrds -out <file> <dir>...")
		os.Exit(2)
	}

	if err := run(*out, flag.Args()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(out string, dirs []string) error {
	var docs []map[string]any

	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return fmt.Errorf("reading %s: %w", dir, err)
		}

		for _, entry := range entries {
			if entry.IsDir() || filepath.Ext(entry.Name()) != ".yaml" {
				continue
			}

			raw, err := os.ReadFile(filepath.Join(dir, entry.Name()))
			if err != nil {
				return fmt.Errorf("reading %s: %w", entry.Name(), err)
			}

			doc := map[string]any{}
			if err := yaml.Unmarshal(raw, &doc); err != nil {
				return fmt.Errorf("parsing %s: %w", entry.Name(), err)
			}
			if err := Rename(doc); err != nil {
				return fmt.Errorf("renaming %s: %w", entry.Name(), err)
			}

			docs = append(docs, doc)
		}
	}

	// A stable order, or every run produces a different file.
	slices.SortFunc(docs, func(a, b map[string]any) int {
		return strings.Compare(name(a), name(b))
	})

	var buf bytes.Buffer
	for _, doc := range docs {
		encoded, err := marshal(doc)
		if err != nil {
			return fmt.Errorf("encoding %s: %w", name(doc), err)
		}

		buf.WriteString("---\n")
		buf.Write(encoded)
	}

	if err := checkNoLeftoverUpstream(out, buf.Bytes()); err != nil {
		return err
	}

	return os.WriteFile(out, buf.Bytes(), 0o644)
}

// marshal renders a document with the two-space indent kubectl and
// controller-gen use, so the committed files carry no unrelated whitespace
// diff on top of the actual rename: the library's own default is four spaces.
func marshal(doc map[string]any) ([]byte, error) {
	var buf bytes.Buffer

	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)

	if err := enc.Encode(doc); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

func name(doc map[string]any) string {
	metadata, _ := doc["metadata"].(map[string]any)
	value, _ := metadata["name"].(string)

	return value
}

// checkNoLeftoverUpstream fails if the rendered output still names an upstream
// flux group or kind: Rename addresses identities it knows by name, so a kind
// added by a future upstream release, or a group nested somewhere the walk
// does not visit, would otherwise slip through and ship silently unrenamed.
func checkNoLeftoverUpstream(path string, rendered []byte) error {
	for i, line := range strings.Split(string(rendered), "\n") {
		stripped := strings.ReplaceAll(line, allowedAnnotation, "")
		if !strings.Contains(stripped, forbiddenGroup) {
			continue
		}

		return fmt.Errorf("%s:%d: leftover upstream identity: %s", path, i+1, strings.TrimSpace(line))
	}

	return nil
}
