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
	"sync"
)

// ErrBudgetExhausted is returned by Put when storing an entry would take the
// cache over its budget. Callers are expected to log it and carry on.
var ErrBudgetExhausted = errors.New("cache budget exhausted")

// Cache is an in-memory cache for extracted values.yaml files, keyed by the auxiliary
// resource name both the HTTP handler and the controller derive and by the artifact revision.
type Cache struct {
	mu      sync.Mutex
	budget  int64
	used    int64
	entries map[string]map[string][]byte
}

// New returns an empty cache that holds at most budgetBytes of values.
func New(budgetBytes int64) *Cache {
	return &Cache{budget: budgetBytes, entries: make(map[string]map[string][]byte)}
}

// Get returns the cached values for a key when they were extracted from the artifact
// revision with the given digest, or ok=false otherwise. An empty digest always misses.
func (c *Cache) Get(key, digest string) ([]byte, bool) {
	if digest == "" {
		return nil, false
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	values, ok := c.entries[key][digest]

	return values, ok
}

// Put stores values extracted from the artifact revision with the given digest under a key,
// replacing only the entry of that revision. Over budget it returns ErrBudgetExhausted and
// keeps the old entry.
func (c *Cache) Put(key, digest string, values []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	next := c.used - int64(len(c.entries[key][digest])) + int64(len(values))
	if next > c.budget {
		return fmt.Errorf("caching %s: %w", key, ErrBudgetExhausted)
	}

	if c.entries[key] == nil {
		c.entries[key] = make(map[string][]byte)
	}
	c.entries[key][digest] = bytes.Clone(values)
	c.used = next

	return nil
}

// Delete removes every revision stored under a key and gives the space they took back to the budget.
func (c *Cache) Delete(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	for _, values := range c.entries[key] {
		c.used -= int64(len(values))
	}
	delete(c.entries, key)
}
