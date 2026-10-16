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

package repository

import (
	"bytes"
	"errors"
	"fmt"

	"go.yaml.in/yaml/v3"
)

// Variables rather than constants so the tests that pin them need not build a document of that size.
var (
	maxIndexEntrySize = 1 << 20   // Bounds one chart version of a large index.
	maxWholeIndexSize = 2 << 20   // Up to this size an index in any layout is decoded as one document.
	indexBatchSize    = 256 << 10 // Versions of one chart are decoded together once they amount to this.
)

var errIndexLayout = errors.New("index.yaml is not in the block layout helm writes")

// decodeIndex reads an index a few chart versions at a time: a document decoded whole keeps a
// node tree some fifty times its size, so an index of tens of MiB would cost gigabytes. The block
// layout helm writes lets each version be cut out by indentation and decoded in small batches.
//
// shortcut: an index over maxWholeIndexSize must be in that layout and use no anchors, since a batch
// is decoded on its own; a smaller one in any layout is decoded whole. Upgrade to an event-based
// parser if a producer in the wild emits large indexes outside it.
func decodeIndex(body []byte) (HelmRepositoryIndex, error) {
	index, err := decodeIndexByEntry(body)
	if err == nil {
		return index, nil
	}

	if len(body) > maxWholeIndexSize {
		return HelmRepositoryIndex{}, err
	}

	var whole HelmRepositoryIndex
	if err := yaml.Unmarshal(body, &whole); err != nil {
		return HelmRepositoryIndex{}, err
	}

	return whole, nil
}

func decodeIndexByEntry(body []byte) (HelmRepositoryIndex, error) {
	var (
		index       HelmRepositoryIndex
		started     bool // A document marker after this one ends the index, as a whole decode would.
		marked      bool // A second marker before the content starts does too: the first document is empty.
		seenTop     = map[string]struct{}{}
		top         []byte // The top-level block not decoded yet; the entries block never lands here.
		topKey      string
		inEntries   bool
		chart       string
		chartClosed bool // The chart carried its versions inline, so no item may follow.
		chartIndent = -1
		itemIndent  = -1
		batch       []byte // The versions of the current chart not decoded yet.
		itemStart   int    // Where the last of them begins in batch.
	)

	grow := func(buf *[]byte, line []byte, start int, what string) error {
		*buf = append(*buf, line...)
		if len(*buf)-start > maxIndexEntrySize {
			return fmt.Errorf("%w: %s exceeds %d MiB", errIndexTooLarge, what, maxIndexEntrySize>>20)
		}

		return nil
	}

	flushTop := func() error {
		if top == nil {
			return nil
		}

		// Decoded into the index type so that apiVersion is checked and converted as a whole decode would.
		var block HelmRepositoryIndex
		if err := yaml.Unmarshal(top, &block); err != nil {
			return err
		}

		if topKey == "apiVersion" {
			index.APIVersion = block.APIVersion
		}

		top = nil

		return nil
	}

	flushBatch := func() error {
		if batch == nil {
			return nil
		}

		var versions []HelmRepositoryChartVersion
		if err := yaml.Unmarshal(batch, &versions); err != nil {
			return fmt.Errorf("chart %s: %w", chart, err)
		}

		index.Entries[chart] = append(index.Entries[chart], versions...)
		batch = nil

		return nil
	}

	// singleKey decodes one mapping line into its only key and value node.
	singleKey := func(line []byte) (string, yaml.Node, error) {
		var key map[string]yaml.Node
		if err := yaml.Unmarshal(line, &key); err != nil {
			return "", yaml.Node{}, err
		}
		if len(key) != 1 {
			return "", yaml.Node{}, fmt.Errorf("%w: %s", errIndexLayout, bytes.TrimSpace(line))
		}

		for name, node := range key {
			return name, node, nil
		}

		return "", yaml.Node{}, nil
	}

	// open reports whether a key's value is still to come on the following lines.
	open := func(node yaml.Node) bool {
		return node.Tag == "!!null" && node.Value == ""
	}

	for len(body) > 0 {
		var line []byte

		if i := bytes.IndexByte(body, '\n'); i >= 0 {
			line, body = body[:i+1], body[i+1:]
		} else {
			line, body = body, nil
		}

		if len(line) > maxIndexEntrySize {
			if chart != "" {
				return HelmRepositoryIndex{}, fmt.Errorf("%w: a line of chart %q exceeds %d MiB", errIndexTooLarge, chart, maxIndexEntrySize>>20)
			}

			return HelmRepositoryIndex{}, fmt.Errorf("%w: a line exceeds %d MiB", errIndexTooLarge, maxIndexEntrySize>>20)
		}

		content := bytes.TrimLeft(line, " ")
		indent := len(line) - len(content)
		trimmed := bytes.TrimRight(content, "\r\n")

		if len(trimmed) == 0 || trimmed[0] == '#' {
			var err error

			switch {
			case batch != nil:
				err = grow(&batch, line, itemStart, fmt.Sprintf("a version of chart %q", chart))
			case top != nil:
				err = grow(&top, line, 0, "a top-level value")
			}

			if err != nil {
				return HelmRepositoryIndex{}, err
			}

			continue
		}

		if indent == 0 {
			if trimmed[0] == '%' {
				// A directive belongs before the document; anywhere else it is a syntax error.
				if started || marked {
					return HelmRepositoryIndex{}, fmt.Errorf("%w: %s", errIndexLayout, trimmed)
				}

				continue
			}

			if bytes.HasPrefix(trimmed, []byte("---")) || bytes.HasPrefix(trimmed, []byte("...")) {
				rest := bytes.TrimLeft(trimmed[3:], " \t")
				if len(rest) != 0 && rest[0] != '#' {
					return HelmRepositoryIndex{}, fmt.Errorf("%w: %s", errIndexLayout, trimmed)
				}

				if started || marked {
					break
				}

				marked = true

				continue
			}

			started = true

			if err := errors.Join(flushBatch(), flushTop()); err != nil {
				return HelmRepositoryIndex{}, err
			}

			inEntries, chart, chartIndent, itemIndent = false, "", -1, -1

			name, node, err := singleKey(line)
			if err != nil {
				return HelmRepositoryIndex{}, err
			}

			if _, dup := seenTop[name]; dup {
				return HelmRepositoryIndex{}, fmt.Errorf("%w: key %q is listed twice", errIndexLayout, name)
			}

			seenTop[name] = struct{}{}

			if name != "entries" {
				top, topKey = append([]byte{}, line...), name

				continue
			}

			switch {
			case open(node):
				inEntries = true
			case node.Tag == "!!null":
			case node.Kind == yaml.MappingNode && len(node.Content) == 0:
				index.Entries = map[string][]HelmRepositoryChartVersion{}
			default:
				return HelmRepositoryIndex{}, errIndexLayout
			}

			continue
		}

		if !inEntries {
			if top == nil {
				return HelmRepositoryIndex{}, fmt.Errorf("%w: %s", errIndexLayout, trimmed)
			}

			if err := grow(&top, line, 0, "a top-level value"); err != nil {
				return HelmRepositoryIndex{}, err
			}

			continue
		}

		isItem := trimmed[0] == '-' && (len(trimmed) == 1 || trimmed[1] == ' ')

		switch {
		case isItem && chart != "" && !chartClosed && (itemIndent == -1 || indent == itemIndent) && indent >= chartIndent:
			if len(batch) >= indexBatchSize {
				if err := flushBatch(); err != nil {
					return HelmRepositoryIndex{}, err
				}
			}

			itemIndent, itemStart = indent, len(batch)
			batch = append(batch, line...)
		case batch != nil && indent > itemIndent:
			if err := grow(&batch, line, itemStart, fmt.Sprintf("a version of chart %q", chart)); err != nil {
				return HelmRepositoryIndex{}, err
			}
		case !isItem && (chartIndent == -1 || indent == chartIndent):
			if err := flushBatch(); err != nil {
				return HelmRepositoryIndex{}, err
			}

			name, node, err := singleKey(trimmed)
			if err != nil {
				return HelmRepositoryIndex{}, fmt.Errorf("%w: %s", errIndexLayout, trimmed)
			}

			if index.Entries == nil {
				index.Entries = map[string][]HelmRepositoryChartVersion{}
			}

			if _, dup := index.Entries[name]; dup {
				return HelmRepositoryIndex{}, fmt.Errorf("%w: chart %q is listed twice", errIndexLayout, name)
			}

			var versions []HelmRepositoryChartVersion
			if !open(node) {
				if err := node.Decode(&versions); err != nil {
					return HelmRepositoryIndex{}, fmt.Errorf("chart %s: %w", name, err)
				}
			}

			chart, chartClosed, chartIndent, itemIndent = name, !open(node), indent, -1
			index.Entries[chart] = versions
		default:
			return HelmRepositoryIndex{}, fmt.Errorf("%w: %s", errIndexLayout, trimmed)
		}
	}

	if err := errors.Join(flushBatch(), flushTop()); err != nil {
		return HelmRepositoryIndex{}, err
	}

	return index, nil
}
