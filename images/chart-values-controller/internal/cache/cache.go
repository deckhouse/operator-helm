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
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Cache is an on-disk cache for extracted values.yaml files, backed by an
// emptyDir volume (per-pod, non-persistent). Entries are keyed by the auxiliary
// resource name (a deterministic hash of the repository/chart/version triple),
// so both the HTTP handler and the auxiliary-resource controller derive the same
// key; the controller keeps each entry in sync with the current artifact.
type Cache struct {
	dir string
}

func New(dir string) *Cache {
	return &Cache{dir: dir}
}

func (c *Cache) path(key string) string {
	return filepath.Join(c.dir, strings.ReplaceAll(key, ":", "_"))
}

// Get returns the cached content for a key, or ok=false on a miss.
func (c *Cache) Get(key string) ([]byte, bool) {
	data, err := os.ReadFile(c.path(key))
	if err != nil {
		return nil, false
	}

	return data, true
}

// Put stores content for a key, writing atomically via a unique temp file +
// rename. Concurrent writers of the same key each use their own temp file, so no
// external locking is needed: the atomic rename publishes a whole file and, for a
// given key, the content is identical (same artifact digest) regardless of order.
func (c *Cache) Put(key string, content []byte) error {
	if err := os.MkdirAll(c.dir, 0o755); err != nil {
		return fmt.Errorf("creating cache dir: %w", err)
	}

	tmp, err := os.CreateTemp(c.dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("creating temp cache file: %w", err)
	}
	tmpName := tmp.Name()
	// Best-effort cleanup: harmless no-op once the temp file has been renamed.
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := tmp.Write(content); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("writing cache entry: %w", err)
	}

	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing cache entry: %w", err)
	}

	if err := os.Rename(tmpName, c.path(key)); err != nil {
		return fmt.Errorf("committing cache entry: %w", err)
	}

	return nil
}

// Delete removes the cache entry for a key. A missing entry is not an error.
func (c *Cache) Delete(key string) error {
	if err := os.Remove(c.path(key)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing cache entry: %w", err)
	}

	return nil
}
