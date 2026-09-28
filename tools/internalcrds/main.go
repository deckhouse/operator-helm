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

// Command internalcrds turns the upstream flux CustomResourceDefinitions into the internal
// ones this module ships, reading every yaml document of the input directories into one file.
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

// forbiddenGroup is the upstream group that must not survive into the rendered output.
const forbiddenGroup = "toolkit.fluxcd.io"

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
			if err := checkKnownKind(entry.Name(), doc); err != nil {
				return err
			}
			if err := Rename(doc); err != nil {
				return fmt.Errorf("renaming %s: %w", entry.Name(), err)
			}

			docs = append(docs, doc)
		}
	}

	// Sort by object name so that repeated runs produce an identical file.
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

// marshal renders a document with the two-space indent kubectl and controller-gen use.
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

// checkKnownKind fails when a document declares a kind that upstreamKinds does not list.
func checkKnownKind(entryName string, doc map[string]any) error {
	spec, _ := doc["spec"].(map[string]any)
	names, _ := spec["names"].(map[string]any)
	kind, _ := names["kind"].(string)

	if !slices.Contains(upstreamKinds, kind) {
		return fmt.Errorf("%s: unknown upstream kind %q", entryName, kind)
	}

	return nil
}

// checkNoLeftoverUpstream fails when the rendered output still names an upstream flux group or kind.
func checkNoLeftoverUpstream(path string, rendered []byte) error {
	for i, line := range strings.Split(string(rendered), "\n") {
		if !strings.Contains(line, forbiddenGroup) && !kindWord.MatchString(line) {
			continue
		}

		return fmt.Errorf("%s:%d: leftover upstream identity: %s", path, i+1, strings.TrimSpace(line))
	}

	return nil
}
