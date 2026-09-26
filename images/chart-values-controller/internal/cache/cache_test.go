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
	"sync"
	"sync/atomic"
	"testing"
)

// testBudget is far above anything the round-trip tests store, so only the tests
// that set their own budget exercise the accounting.
const testBudget = int64(16 << 20)

func TestPutGetRoundTrip(t *testing.T) {
	c := New(t.TempDir(), testBudget)

	if _, ok := c.Get("cv-abc"); ok {
		t.Fatal("expected miss on empty cache")
	}

	want := []byte("replicaCount: 1\n")
	if err := c.Put("cv-abc", want); err != nil {
		t.Fatalf("put: %v", err)
	}

	got, ok := c.Get("cv-abc")
	if !ok || !bytes.Equal(got, want) {
		t.Fatalf("get = (%q, %v), want (%q, true)", got, ok, want)
	}

	if err := c.Delete("cv-abc"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, ok := c.Get("cv-abc"); ok {
		t.Fatal("expected miss after delete")
	}
}

// TestConcurrentSameKey exercises the -race detector: many writers of the same
// key (same content, as the digest guarantees) plus concurrent readers must
// never observe a partial or corrupted file.
func TestConcurrentSameKey(t *testing.T) {
	c := New(t.TempDir(), testBudget)

	const key = "cv-concurrent"
	want := bytes.Repeat([]byte("a: b\n"), 4096)

	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			if err := c.Put(key, want); err != nil {
				t.Errorf("put: %v", err)
			}
		}()
		go func() {
			defer wg.Done()
			if got, ok := c.Get(key); ok && !bytes.Equal(got, want) {
				t.Errorf("get returned corrupted content: %d bytes", len(got))
			}
		}()
	}
	wg.Wait()

	got, ok := c.Get(key)
	if !ok || !bytes.Equal(got, want) {
		t.Fatalf("final get = (%d bytes, %v), want (%d bytes, true)", len(got), ok, len(want))
	}
}

// TestPutRefusesAnEntryThatDoesNotFitTheBudget covers the whole point of the
// budget: the volume's sizeLimit only gets the pod evicted, so the cache has to
// say no by itself, and say it without losing what is already stored. The budget
// is charged twice per write because an atomic write briefly holds a temp copy
// next to the published one.
func TestPutRefusesAnEntryThatDoesNotFitTheBudget(t *testing.T) {
	// One block's worth of content, since a budget is spent in whole blocks: this
	// is what an entry costs the volume however few bytes it holds.
	content := bytes.Repeat([]byte("x"), 1024)

	tests := []struct {
		name       string
		budget     int64
		wantStored bool
	}{
		{
			name:       "budget below one block is refused outright",
			budget:     blockSize - 1,
			wantStored: false,
		},
		{
			name:       "budget covering only the final file still leaves the temp copy unpaid for",
			budget:     blockSize,
			wantStored: false,
		},
		{
			name:       "budget covering the temp copy and the final file admits the write",
			budget:     2 * blockSize,
			wantStored: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			c := New(dir, tt.budget)

			err := c.Put("cv-fit", content)
			if tt.wantStored {
				if err != nil {
					t.Fatalf("put: %v", err)
				}
				if got, ok := c.Get("cv-fit"); !ok || !bytes.Equal(got, content) {
					t.Fatal("entry that fits the budget must be readable back")
				}
				return
			}

			if !errors.Is(err, ErrBudgetExhausted) {
				t.Fatalf("put = %v, want ErrBudgetExhausted", err)
			}
			if _, ok := c.Get("cv-fit"); ok {
				t.Fatal("a refused write must not leave an entry behind")
			}
			if n := dirBytes(t, dir); n != 0 {
				t.Fatalf("refused write left %d bytes on disk, want 0", n)
			}
		})
	}
}

// TestPutKeepsServingEarlierEntriesOnceTheBudgetIsExhausted pins the chosen
// degradation: the cache refuses new entries instead of evicting old ones, so a
// full cache still answers every request it could answer before. The values of a
// refused write are handed to the client by the caller regardless.
func TestPutKeepsServingEarlierEntriesOnceTheBudgetIsExhausted(t *testing.T) {
	content := bytes.Repeat([]byte("y"), 512)
	// Room for exactly one entry: 2n is claimed while writing, n stays afterwards,
	// which leaves n free — not the 2n the next write needs.
	c := New(t.TempDir(), 2*blockSize)

	if err := c.Put("cv-first", content); err != nil {
		t.Fatalf("first put: %v", err)
	}
	if err := c.Put("cv-second", content); !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("second put = %v, want ErrBudgetExhausted", err)
	}

	if got, ok := c.Get("cv-first"); !ok || !bytes.Equal(got, content) {
		t.Fatal("the entry stored before the budget ran out must survive: nothing is evicted")
	}
}

// TestOverwritingAnEntryCountsItOnce guards the accounting against charging the
// same key twice: the controller rewrites an entry on every revision change, and a
// cache that never gave the old bytes back would refuse everything after a while.
func TestOverwritingAnEntryCountsItOnce(t *testing.T) {
	content := bytes.Repeat([]byte("z"), 512)
	c := New(t.TempDir(), 2*blockSize)

	for i := range 5 {
		if err := c.Put("cv-same", content); err != nil {
			t.Fatalf("put %d: %v", i, err)
		}
	}

	if got, ok := c.Get("cv-same"); !ok || !bytes.Equal(got, content) {
		t.Fatal("rewriting one key must keep it readable")
	}
}

// TestDeleteGivesTheSpaceBackToTheBudget ties the budget to the controller's
// expiry pass: it deletes the auxiliary object together with its cache entry, and
// if that did not free the space the cache would stay full for the life of the pod.
func TestDeleteGivesTheSpaceBackToTheBudget(t *testing.T) {
	content := bytes.Repeat([]byte("q"), 512)
	c := New(t.TempDir(), 2*blockSize)

	if err := c.Put("cv-old", content); err != nil {
		t.Fatalf("put: %v", err)
	}
	if err := c.Put("cv-new", content); !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("put into a full cache = %v, want ErrBudgetExhausted", err)
	}

	if err := c.Delete("cv-old"); err != nil {
		t.Fatalf("delete: %v", err)
	}

	if err := c.Put("cv-new", content); err != nil {
		t.Fatalf("put after delete: %v", err)
	}

	// A delete of a key that is not there must not credit the budget a second time.
	if err := c.Delete("cv-old"); err != nil {
		t.Fatalf("delete of a missing entry: %v", err)
	}
	if err := c.Put("cv-third", content); !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("put = %v, want ErrBudgetExhausted: deleting nothing frees nothing", err)
	}
}

// TestNewAccountsForEntriesAlreadyOnDisk covers a restarted container: the
// emptyDir keeps its files, and a budget starting from zero would allow a second
// volume's worth of entries on top of them.
func TestNewAccountsForEntriesAlreadyOnDisk(t *testing.T) {
	content := bytes.Repeat([]byte("w"), 512)

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "cv-survivor"), content, 0o644); err != nil {
		t.Fatalf("seeding the cache dir: %v", err)
	}

	// Enough for the pre-existing entry and for one more write (2n while it runs).
	if err := New(dir, 3*blockSize).Put("cv-fresh", content); err != nil {
		t.Fatalf("put with room for both: %v", err)
	}
	if err := os.Remove(filepath.Join(dir, "cv-fresh")); err != nil {
		t.Fatalf("clearing the fresh entry: %v", err)
	}

	// One byte short of that, which only shows if the pre-existing file is counted.
	if err := New(dir, 3*blockSize-1).Put("cv-fresh", content); !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("put = %v, want ErrBudgetExhausted: files found at startup must be charged to the budget", err)
	}
}

// TestNewSweepsTempFilesLeftByACrash covers the other half of startup: a temp file
// can only be left by a process that died mid-write (one process owns the
// directory), it will never be renamed into an entry, and if it were merely
// counted it would hold the space until the pod is replaced.
func TestNewSweepsTempFilesLeftByACrash(t *testing.T) {
	content := bytes.Repeat([]byte("e"), 512)

	dir := t.TempDir()
	orphan := filepath.Join(dir, tempPrefix+"12345")
	if err := os.WriteFile(orphan, content, 0o644); err != nil {
		t.Fatalf("seeding a leftover temp file: %v", err)
	}

	c := New(dir, 2*blockSize)
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatalf("stat of the leftover temp file = %v, want it removed", err)
	}
	if err := c.Put("cv-after-crash", content); err != nil {
		t.Fatalf("put: %v", err)
	}
}

// TestConcurrentPutsOfDifferentKeysStayWithinTheBudget is the -race case that
// matters: only same-key work is collapsed upstream (resolver.inflight), so
// different keys stage their temp files side by side and the budget is the only
// thing keeping their sum off the volume's sizeLimit.
func TestConcurrentPutsOfDifferentKeysStayWithinTheBudget(t *testing.T) {
	content := bytes.Repeat([]byte("k"), 4096)

	const writers = 16
	// Room for four entries once settled, for two writes at their peak.
	budget := int64(4 * blockSize)
	dir := t.TempDir()
	c := New(dir, budget)

	var (
		wg      sync.WaitGroup
		stored  atomic.Int64
		refused atomic.Int64
	)
	for i := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()

			err := c.Put(fmt.Sprintf("cv-%d", i), content)
			switch {
			case err == nil:
				stored.Add(1)
			case errors.Is(err, ErrBudgetExhausted):
				refused.Add(1)
			default:
				t.Errorf("put: %v", err)
			}
		}()
	}
	wg.Wait()

	if stored.Load() == 0 {
		t.Fatal("no writer got in: a budget with room for four entries must admit somebody")
	}
	if refused.Load() == 0 {
		t.Fatalf("all %d writers got in: the budget must stop some of them", writers)
	}
	if n := dirBytes(t, dir); n > budget {
		t.Fatalf("cache dir holds %d bytes, over the %d-byte budget", n, budget)
	}

	// Whatever was admitted must be a whole, readable entry, never a partial one.
	for i := range writers {
		if got, ok := c.Get(fmt.Sprintf("cv-%d", i)); ok && !bytes.Equal(got, content) {
			t.Fatalf("entry cv-%d holds %d bytes, want the full content", i, len(got))
		}
	}
}

// dirBytes sums the entries of a cache directory, temp files included: they
// occupy the volume just as much as the published ones.
func dirBytes(t *testing.T, dir string) int64 {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading cache dir: %v", err)
	}

	var total int64
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			t.Fatalf("stat %s: %v", e.Name(), err)
		}
		total += info.Size()
	}

	return total
}

// TestPutAndDeleteOfOneKeyLeaveTheAccountingMatchingTheDisk is the regression for
// the window between publishing an entry and recording it. A delete landing in
// that window used to subtract a file that was about to exist, leaving the entry
// on the volume with nothing charged for it — or charged for one that is gone.
// Neither is visible to the race detector: both interleavings are properly
// synchronized, they just disagreed about the world.
func TestPutAndDeleteOfOneKeyLeaveTheAccountingMatchingTheDisk(t *testing.T) {
	content := bytes.Repeat([]byte("r"), 1024)

	for range 200 {
		dir := t.TempDir()
		c := New(dir, 64*blockSize)

		var (
			wg                sync.WaitGroup
			putErr, deleteErr error
		)
		wg.Add(2)

		go func() {
			defer wg.Done()
			putErr = c.Put("cv-contended", content)
		}()
		go func() {
			defer wg.Done()
			deleteErr = c.Delete("cv-contended")
		}()

		wg.Wait()

		if putErr != nil {
			t.Fatalf("put: %v", putErr)
		}
		if deleteErr != nil {
			t.Fatalf("delete: %v", deleteErr)
		}

		_, present := c.Get("cv-contended")

		c.mu.Lock()
		charged, reserved, recorded := c.used, c.reserved, c.sizes["cv-contended"]
		c.mu.Unlock()

		if reserved != 0 {
			t.Fatalf("reserved = %d after both finished, want 0", reserved)
		}

		// Either order is a correct outcome; what has to hold is that the two
		// agree, down to the exact block the entry costs.
		want := int64(0)
		if present {
			want = blockSize
		}
		if charged != want || recorded != want {
			t.Fatalf("entry on the volume: %t, but used = %d and sizes = %d, want %d for both",
				present, charged, recorded, want)
		}
	}
}
