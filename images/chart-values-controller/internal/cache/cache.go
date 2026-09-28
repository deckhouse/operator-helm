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

package cache

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const blockSize = 4096

func onDisk(n int64) int64 {
	return (n + blockSize - 1) / blockSize * blockSize
}

const tempPrefix = ".tmp-"

const entryHeaderPrefix = "#chart-values-cache/v1 digest="

// DigestUnknown is the revision of an entry that does not say which artifact it came
// from. It is not a digest any artifact can have, so it never equals a current one.
const DigestUnknown = ""

// Entry is what is stored under a key: the extracted values.yaml together with the
// digest of the artifact revision they were extracted from.
type Entry struct {
	Values []byte
	Digest string
}

func encodeEntry(digest string, content []byte) []byte {
	// A digest carrying a newline would forge a header, so it is refused into unknown.
	if strings.ContainsAny(digest, "\r\n") {
		digest = DigestUnknown
	}

	out := make([]byte, 0, len(entryHeaderPrefix)+len(digest)+1+len(content))
	out = append(out, entryHeaderPrefix...)
	out = append(out, digest...)
	out = append(out, '\n')

	return append(out, content...)
}

func decodeEntry(raw []byte) Entry {
	if !bytes.HasPrefix(raw, []byte(entryHeaderPrefix)) {
		return Entry{Values: raw, Digest: DigestUnknown}
	}

	rest := raw[len(entryHeaderPrefix):]

	line := bytes.IndexByte(rest, '\n')
	if line < 0 {
		return Entry{Values: raw, Digest: DigestUnknown}
	}

	return Entry{Values: rest[line+1:], Digest: string(rest[:line])}
}

// ErrBudgetExhausted is returned by Put when storing an entry would take the
// directory over its budget. Callers are expected to log it and carry on.
var ErrBudgetExhausted = errors.New("cache budget exhausted")

// Cache is an on-disk cache for extracted values.yaml files on an emptyDir volume,
// keyed by the auxiliary resource name both the HTTP handler and the controller derive.
type Cache struct {
	dir string
	// budget is what committed entries plus the peak of the writes in flight may reach.
	budget int64

	mu sync.Mutex
	// used is the summed size of the files accounted under dir, sizes their per-file breakdown.
	used  int64
	sizes map[string]int64
	// reserved covers the writes in flight: a claim is the staged temp file plus the entry's growth,
	// which for a new key is twice its on-disk size, though only one file is written before the rename.
	reserved int64
}

// New opens a cache directory with a budget in bytes and takes stock of what is
// already there, which a container restart leaves in place.
func New(dir string, budgetBytes int64) *Cache {
	c := &Cache{dir: dir, budget: budgetBytes, sizes: make(map[string]int64)}
	c.scan()

	return c
}

func (c *Cache) scan() {
	entries, err := os.ReadDir(c.dir)
	if err != nil {
		return
	}

	for _, e := range entries {
		if e.IsDir() {
			continue
		}

		if strings.HasPrefix(e.Name(), tempPrefix) {
			if err := os.Remove(filepath.Join(c.dir, e.Name())); err == nil {
				continue
			}

			// A temp file that will not go away is charged under its own name.
			info, err := e.Info()
			if err != nil {
				continue
			}

			c.sizes[e.Name()] = onDisk(info.Size())
			c.used += onDisk(info.Size())

			continue
		}

		info, err := e.Info()
		if err != nil {
			continue
		}

		c.sizes[e.Name()] = onDisk(info.Size())
		c.used += onDisk(info.Size())
	}
}

func (c *Cache) fileName(key string) string {
	return strings.ReplaceAll(key, ":", "_")
}

func (c *Cache) path(key string) string {
	return filepath.Join(c.dir, c.fileName(key))
}

// Get returns the cached content for a key, or ok=false on a miss.
func (c *Cache) Get(key string) ([]byte, bool) {
	entry, ok := c.GetEntry(key)
	if !ok {
		return nil, false
	}

	return entry.Values, true
}

// GetEntry returns the cached content for a key together with the artifact revision
// it was extracted from, or ok=false on a miss.
func (c *Cache) GetEntry(key string) (Entry, bool) {
	raw, err := os.ReadFile(c.path(key))
	if err != nil {
		return Entry{}, false
	}

	return decodeEntry(raw), true
}

func (c *Cache) reserve(name string, n int64) (int64, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	n = onDisk(n)
	claim := n + max(0, n-c.sizes[name])
	if c.used+c.reserved+claim > c.budget {
		return 0, false
	}

	c.reserved += claim

	return claim, true
}

func (c *Cache) release(claim int64) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.reserved -= claim
}

// Put stores content for a key together with the digest of the artifact revision it was extracted
// from, atomically via a temp file and a rename. Over budget it returns ErrBudgetExhausted.
func (c *Cache) Put(key, digest string, content []byte) error {
	return c.PutUnless(key, digest, content, nil)
}

// ErrSuperseded is returned by PutUnless when the guard refused the entry between the
// temp file and the rename. Nothing was published and the budget is left as it was.
var ErrSuperseded = errors.New("cache entry superseded")

// PutUnless is Put with a guard on whether the entry still deserves publishing, asked under the
// lock the rename happens beneath. The guard reads a synced informer, not the cache or the network.
func (c *Cache) PutUnless(key, digest string, content []byte, guard func() bool) error {
	stored := encodeEntry(digest, content)

	size := int64(len(stored))
	name := c.fileName(key)

	claim, ok := c.reserve(name, size)
	if !ok {
		return fmt.Errorf("caching %s: %w", key, ErrBudgetExhausted)
	}

	committed := false
	defer func() {
		if !committed {
			c.release(claim)
		}
	}()

	if err := os.MkdirAll(c.dir, 0o755); err != nil {
		return fmt.Errorf("creating cache dir: %w", err)
	}

	tmp, err := os.CreateTemp(c.dir, tempPrefix+"*")
	if err != nil {
		return fmt.Errorf("creating temp cache file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := tmp.Write(stored); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("writing cache entry: %w", err)
	}

	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing cache entry: %w", err)
	}

	// The rename and the accounting it implies happen under one hold of the lock.
	c.mu.Lock()
	defer c.mu.Unlock()

	if guard != nil && !guard() {
		return fmt.Errorf("caching %s: %w", key, ErrSuperseded)
	}

	if err := os.Rename(tmpName, c.path(key)); err != nil {
		return fmt.Errorf("committing cache entry: %w", err)
	}

	size = onDisk(size)
	c.reserved -= claim
	c.used += size - c.sizes[name]
	c.sizes[name] = size
	committed = true

	return nil
}

// Delete removes the cache entry for a key and gives the space it took back to the
// budget. A missing entry is not an error.
func (c *Cache) Delete(key string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if err := os.Remove(c.path(key)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing cache entry: %w", err)
	}

	name := c.fileName(key)
	c.used -= c.sizes[name]
	delete(c.sizes, name)

	return nil
}
