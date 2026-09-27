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

// blockSize is what a file costs the volume beyond its own length. A filesystem
// hands out whole blocks, so an entry of a few hundred bytes still takes one, and
// a budget counting content alone would admit several times more entries than the
// volume holds — which is the eviction this budget exists to prevent. 4 KiB is the
// usual block of the filesystems an emptyDir is backed by; being wrong about it
// costs some unused space, while ignoring it costs the pod.
const blockSize = 4096

// onDisk rounds a length up to what it occupies.
func onDisk(n int64) int64 {
	return (n + blockSize - 1) / blockSize * blockSize
}

// tempPrefix marks a staging file of an atomic write. One process owns the cache
// directory (single replica, no leader election), so at construction time no temp
// file can belong to a live writer: whatever is left carries a crash of the
// previous pod and is swept, otherwise it would occupy the volume forever without
// ever being readable.
const tempPrefix = ".tmp-"

// entryHeaderPrefix opens the one line an entry file carries ahead of the values:
// the digest of the artifact those values were extracted from. The revision is kept
// inside the entry file rather than beside it because it is the fact that makes the
// bytes interpretable — a reader must never see values with somebody else's digest,
// nor a digest without its values, and only one file can be published by one rename.
// Storing it in the file name instead would key an entry on two things at once and
// leave the previous name behind on every revision change.
//
// A values.yaml is a comment away from this line, so the marker is written by this
// cache only; a file that does not start with it was written by a container image
// from before entries recorded their revision, and its digest reads as unknown.
const entryHeaderPrefix = "#chart-values-cache/v1 digest="

// DigestUnknown is the revision of an entry that does not say which artifact it came
// from: one left on the volume by a previous version of this container. It is not a
// digest any artifact can have, so it never compares equal to a current one.
const DigestUnknown = ""

// Entry is what is stored under a key: the extracted values.yaml together with the
// digest of the artifact revision they were extracted from. The digest is what lets
// a later pass tell an entry that is still current from one the artifact has moved
// off, without downloading the artifact again to compare.
type Entry struct {
	Values []byte
	Digest string
}

// encodeEntry lays out the pair as the single file a rename publishes atomically.
// A digest carrying a newline would forge a header, so it is refused into unknown
// rather than stored: an unknown revision is handled, a forged one is not.
func encodeEntry(digest string, content []byte) []byte {
	if strings.ContainsAny(digest, "\r\n") {
		digest = DigestUnknown
	}

	out := make([]byte, 0, len(entryHeaderPrefix)+len(digest)+1+len(content))
	out = append(out, entryHeaderPrefix...)
	out = append(out, digest...)
	out = append(out, '\n')

	return append(out, content...)
}

// decodeEntry reads the pair back, treating a file without the header as values of
// an unknown revision: that is exactly what an entry written before this format
// looks like, and reporting it as unknown keeps it readable instead of unreadable.
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
// directory over its budget. It is not a request failure: the values have already
// been extracted and are handed to the client regardless, they are simply not kept
// for the next request. Callers are expected to log it and carry on.
var ErrBudgetExhausted = errors.New("cache budget exhausted")

// Cache is an on-disk cache for extracted values.yaml files, backed by an
// emptyDir volume (per-pod, non-persistent). Entries are keyed by the auxiliary
// resource name (a deterministic hash of the repository/chart/version triple),
// so both the HTTP handler and the auxiliary-resource controller derive the same
// key; the controller keeps each entry in sync with the current artifact.
//
// The volume carries a sizeLimit, but that limit is an eviction trigger, not a
// write barrier: filling it makes the kubelet evict the pod, the pod comes back,
// the controller restores every entry that has not expired yet and the volume
// fills again — eviction in a loop instead of degradation. So the cache keeps its
// own budget, sized below the volume's sizeLimit, and refuses to grow past it.
//
// Refusing to cache is chosen over evicting other entries for two reasons. The
// controller only rewrites an entry when the artifact digest moves, so an entry
// evicted behind its back would stay missing until the chart is republished,
// while a write that never happened leaves the controller's digest bookkeeping
// untouched and the next request simply downloads again. And entries expire on
// their own: the auxiliary object carries a TTL and the controller deletes both it
// and its entry, which releases the space without anyone guessing which entry was
// least useful.
type Cache struct {
	dir string
	// budget is the total size of committed entries plus the peak of the writes in
	// flight that this directory is allowed to reach.
	budget int64

	mu sync.Mutex
	// used is the summed size of the files committed under dir, and sizes their
	// per-file breakdown, so a delete or an overwrite gives the space back exactly.
	used  int64
	sizes map[string]int64
	// reserved covers the writes in flight. Concurrent Puts of different keys stage
	// their own temp files at the same time (only same-key work is collapsed, see
	// resolver.inflight), and each of them briefly occupies the volume twice: the
	// temp file next to the entry the rename is about to replace.
	reserved int64
}

// New opens a cache directory with a budget in bytes and takes stock of what is
// already there: a restarted pod keeps its emptyDir only if the pod itself
// survived, but a container restart leaves every file in place, and a budget that
// started from zero would then allow a second volume's worth of entries.
func New(dir string, budgetBytes int64) *Cache {
	c := &Cache{dir: dir, budget: budgetBytes, sizes: make(map[string]int64)}
	c.scan()

	return c
}

// scan accounts for pre-existing entries and sweeps leftover temp files. Errors
// are deliberately swallowed: a missing or unreadable directory is the empty
// cache, and refusing to start over a cache directory would trade a degraded
// cache for a dead API.
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

			// A temp file that will not go away still occupies the volume, and the
			// budget has to know about it even though no key will ever read it.
			// It is charged under its own name, which nothing else writes.
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
// it was extracted from, or ok=false on a miss. The digest is DigestUnknown for an
// entry a previous version of this container left on the volume.
func (c *Cache) GetEntry(key string) (Entry, bool) {
	raw, err := os.ReadFile(c.path(key))
	if err != nil {
		return Entry{}, false
	}

	return decodeEntry(raw), true
}

// reserve admits a write of n bytes and returns what it claimed. A write peaks at
// the staged temp file next to what the rename is about to replace, so the claim
// is n for the temp file plus however much the entry grows by — for a new key,
// 2n; for a refresh of the same chart, close to n, since the bytes of the entry
// being replaced are already counted in used. Charging the peak keeps the
// directory below the budget at every instant, not only once the writes settle.
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

// release gives back a claim of a write that never reached the disk.
func (c *Cache) release(claim int64) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.reserved -= claim
}

// Put stores content for a key together with the digest of the artifact revision it
// was extracted from, writing atomically via a unique temp file + rename. The two
// facts travel in one file, so the rename publishes both or neither and a reader
// never sees values attributed to the wrong revision. Concurrent writers of the same
// key each use their own temp file, so no external locking is needed: the atomic
// rename publishes a whole file and, for a given key, the content is identical (same
// artifact digest) regardless of order.
//
// Every caller passes the digest it fetched, so an entry written by this code always
// names its revision; only a file inherited from an older container image reads back
// as DigestUnknown.
//
// Over budget it returns ErrBudgetExhausted without touching the disk. The caller
// already holds the values it was going to cache, so nothing but the next
// request's latency is lost.
func (c *Cache) Put(key, digest string, content []byte) error {
	// The header is part of what the volume holds, so the budget is charged for the
	// bytes that are actually written, not for the values alone.
	stored := encodeEntry(digest, content)

	size := int64(len(stored))
	name := c.fileName(key)

	claim, ok := c.reserve(name, size)
	if !ok {
		return fmt.Errorf("caching %s: %w", key, ErrBudgetExhausted)
	}

	// Releases the claim on every path that does not reach the rename below; the
	// one that does records the entry itself, under the lock it renames beneath.
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
	// Best-effort cleanup: harmless no-op once the temp file has been renamed.
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := tmp.Write(stored); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("writing cache entry: %w", err)
	}

	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing cache entry: %w", err)
	}

	// The rename and the bookkeeping it implies happen under one hold of the lock.
	// They are two statements about one entry, and either order of interleaving with
	// a Delete leaves them disagreeing: a delete between them subtracts a file that
	// is about to exist, and the entry then sits on the volume unaccounted for until
	// the pod restarts. Writing the temp file, which is the slow part, stays outside.
	c.mu.Lock()
	defer c.mu.Unlock()

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

// Delete removes the cache entry for a key. A missing entry is not an error.
// The space it took is given back to the budget, which is what keeps the
// controller's expiry pass from leaving the cache permanently full.
func (c *Cache) Delete(key string) error {
	// Held across the removal for the same reason Put holds it across the rename:
	// the file and what is recorded about it move together.
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
