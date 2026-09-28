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

const testBudget = int64(16 << 20)

const testDigest = "sha256:0123456789abcdef"

func TestPutGetRoundTrip(t *testing.T) {
	c := New(t.TempDir(), testBudget)

	if _, ok := c.Get("cv-abc"); ok {
		t.Fatal("expected miss on empty cache")
	}

	want := []byte("replicaCount: 1\n")
	if err := c.Put("cv-abc", "sha256:abc", want); err != nil {
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

func TestConcurrentSameKey(t *testing.T) {
	c := New(t.TempDir(), testBudget)

	const key = "cv-concurrent"
	want := bytes.Repeat([]byte("a: b\n"), 4096)

	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			if err := c.Put(key, "sha256:concurrent", want); err != nil {
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

func TestPutRefusesAnEntryThatDoesNotFitTheBudget(t *testing.T) {
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

			err := c.Put("cv-fit", testDigest, content)
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

func TestPutKeepsServingEarlierEntriesOnceTheBudgetIsExhausted(t *testing.T) {
	content := bytes.Repeat([]byte("y"), 512)
	c := New(t.TempDir(), 2*blockSize)

	if err := c.Put("cv-first", testDigest, content); err != nil {
		t.Fatalf("first put: %v", err)
	}
	if err := c.Put("cv-second", testDigest, content); !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("second put = %v, want ErrBudgetExhausted", err)
	}

	if got, ok := c.Get("cv-first"); !ok || !bytes.Equal(got, content) {
		t.Fatal("the entry stored before the budget ran out must survive: nothing is evicted")
	}
}

func TestOverwritingAnEntryCountsItOnce(t *testing.T) {
	content := bytes.Repeat([]byte("z"), 512)
	c := New(t.TempDir(), 2*blockSize)

	for i := range 5 {
		if err := c.Put("cv-same", testDigest, content); err != nil {
			t.Fatalf("put %d: %v", i, err)
		}
	}

	if got, ok := c.Get("cv-same"); !ok || !bytes.Equal(got, content) {
		t.Fatal("rewriting one key must keep it readable")
	}
}

func TestDeleteGivesTheSpaceBackToTheBudget(t *testing.T) {
	content := bytes.Repeat([]byte("q"), 512)
	c := New(t.TempDir(), 2*blockSize)

	if err := c.Put("cv-old", testDigest, content); err != nil {
		t.Fatalf("put: %v", err)
	}
	if err := c.Put("cv-new", testDigest, content); !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("put into a full cache = %v, want ErrBudgetExhausted", err)
	}

	if err := c.Delete("cv-old"); err != nil {
		t.Fatalf("delete: %v", err)
	}

	if err := c.Put("cv-new", testDigest, content); err != nil {
		t.Fatalf("put after delete: %v", err)
	}

	if err := c.Delete("cv-old"); err != nil {
		t.Fatalf("delete of a missing entry: %v", err)
	}
	if err := c.Put("cv-third", testDigest, content); !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("put = %v, want ErrBudgetExhausted: deleting nothing frees nothing", err)
	}
}

func TestNewAccountsForEntriesAlreadyOnDisk(t *testing.T) {
	content := bytes.Repeat([]byte("w"), 512)

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "cv-survivor"), content, 0o644); err != nil {
		t.Fatalf("seeding the cache dir: %v", err)
	}

	if err := New(dir, 3*blockSize).Put("cv-fresh", testDigest, content); err != nil {
		t.Fatalf("put with room for both: %v", err)
	}
	if err := os.Remove(filepath.Join(dir, "cv-fresh")); err != nil {
		t.Fatalf("clearing the fresh entry: %v", err)
	}

	if err := New(dir, 3*blockSize-1).Put("cv-fresh", testDigest, content); !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("put = %v, want ErrBudgetExhausted: files found at startup must be charged to the budget", err)
	}
}

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
	if err := c.Put("cv-after-crash", testDigest, content); err != nil {
		t.Fatalf("put: %v", err)
	}
}

func TestConcurrentPutsOfDifferentKeysStayWithinTheBudget(t *testing.T) {
	content := bytes.Repeat([]byte("k"), 2048)

	const writers = 16
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

			err := c.Put(fmt.Sprintf("cv-%d", i), testDigest, content)
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

	for i := range writers {
		if got, ok := c.Get(fmt.Sprintf("cv-%d", i)); ok && !bytes.Equal(got, content) {
			t.Fatalf("entry cv-%d holds %d bytes, want the full content", i, len(got))
		}
	}
}

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
			putErr = c.Put("cv-contended", testDigest, content)
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

func TestAnEntryNamesTheArtifactRevisionItWasExtractedFrom(t *testing.T) {
	values := []byte("replicaCount: 1\n# a comment that is not a header\n")

	tests := []struct {
		name       string
		seed       func(t *testing.T, dir string)
		digest     string
		wantValues []byte
		wantDigest string
	}{
		{
			name:       "a digest written with the values reads back with them",
			digest:     "sha256:c0ffee",
			wantValues: values,
			wantDigest: "sha256:c0ffee",
		},
		{
			name:       "values whose own first line looks like a comment keep their digest",
			digest:     "sha256:deadbeef",
			wantValues: []byte("# values.yaml\nreplicaCount: 3\n"),
			wantDigest: "sha256:deadbeef",
		},
		{
			name: "an entry left by a previous version of this container has no revision on record",
			seed: func(t *testing.T, dir string) {
				t.Helper()

				if err := os.WriteFile(filepath.Join(dir, "cv-legacy"), values, 0o644); err != nil {
					t.Fatalf("seeding a pre-format entry: %v", err)
				}
			},
			wantValues: values,
			wantDigest: DigestUnknown,
		},
		{
			name:       "a digest that could forge a header line is refused into unknown",
			digest:     "sha256:one\n" + entryHeaderPrefix + "sha256:two",
			wantValues: values,
			wantDigest: DigestUnknown,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			key := "cv-legacy"

			if tt.seed != nil {
				tt.seed(t, dir)
			} else {
				if err := New(dir, testBudget).Put(key, tt.digest, tt.wantValues); err != nil {
					t.Fatalf("put: %v", err)
				}
			}

			entry, ok := New(dir, testBudget).GetEntry(key)
			if !ok {
				t.Fatal("the entry was not found after reopening the directory")
			}
			if !bytes.Equal(entry.Values, tt.wantValues) {
				t.Fatalf("values = %q, want %q", entry.Values, tt.wantValues)
			}
			if entry.Digest != tt.wantDigest {
				t.Fatalf("digest = %q, want %q", entry.Digest, tt.wantDigest)
			}

			got, ok := New(dir, testBudget).Get(key)
			if !ok || !bytes.Equal(got, tt.wantValues) {
				t.Fatalf("get = (%q, %v), want (%q, true)", got, ok, tt.wantValues)
			}
		})
	}
}

func TestTheBudgetChargesTheRecordedRevisionAlongWithTheValues(t *testing.T) {
	content := bytes.Repeat([]byte("v"), blockSize)

	dir := t.TempDir()
	budget := int64(16 * blockSize)
	c := New(dir, budget)

	stored := 0
	for i := range 32 {
		err := c.Put(fmt.Sprintf("cv-%d", i), testDigest, content)
		if errors.Is(err, ErrBudgetExhausted) {
			break
		}
		if err != nil {
			t.Fatalf("put %d: %v", i, err)
		}
		stored++
	}

	if stored == 0 {
		t.Fatal("no entry was admitted at all")
	}

	c.mu.Lock()
	charged, reserved := c.used, c.reserved
	c.mu.Unlock()

	if reserved != 0 {
		t.Fatalf("reserved = %d once every write settled, want 0", reserved)
	}
	if charged > budget {
		t.Fatalf("charged %d against a budget of %d", charged, budget)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading cache dir: %v", err)
	}

	var occupied int64
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			t.Fatalf("stat %s: %v", e.Name(), err)
		}
		if info.Size() <= int64(len(content)) {
			t.Fatalf("entry %s holds %d bytes, want the values plus the line naming their revision", e.Name(), info.Size())
		}
		occupied += onDisk(info.Size())
	}

	if occupied != charged {
		t.Fatalf("the volume holds %d bytes' worth of blocks while the budget is charged %d", occupied, charged)
	}

	if err := c.Put("cv-one-more", testDigest, content); !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("put into the full cache = %v, want ErrBudgetExhausted", err)
	}
}

func TestAnEntryFileThatSaysNothingUsableReadsAsUnknownRatherThanAsAnotherRevision(t *testing.T) {
	tests := []struct {
		name       string
		raw        []byte
		wantValues []byte
		wantDigest string
	}{
		{
			name:       "an empty file has no values and names no revision",
			raw:        nil,
			wantValues: nil,
			wantDigest: DigestUnknown,
		},
		{
			name:       "a file that is the header alone, with no line to end it, names no revision",
			raw:        []byte(entryHeaderPrefix),
			wantValues: []byte(entryHeaderPrefix),
			wantDigest: DigestUnknown,
		},
		{
			name:       "a header naming no digest names no revision",
			raw:        []byte(entryHeaderPrefix + "\nreplicaCount: 1\n"),
			wantValues: []byte("replicaCount: 1\n"),
			wantDigest: DigestUnknown,
		},
		{
			name:       "a header with no values behind it keeps its revision and holds nothing",
			raw:        []byte(entryHeaderPrefix + "sha256:c0ffee\n"),
			wantValues: nil,
			wantDigest: "sha256:c0ffee",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "cv-edge"), tt.raw, 0o644); err != nil {
				t.Fatalf("seeding the entry file: %v", err)
			}

			entry, ok := New(dir, testBudget).GetEntry("cv-edge")
			if !ok {
				t.Fatal("a file on the volume must be readable, not a miss with a reason")
			}
			if !bytes.Equal(entry.Values, tt.wantValues) {
				t.Fatalf("values = %q, want %q", entry.Values, tt.wantValues)
			}
			if entry.Digest != tt.wantDigest {
				t.Fatalf("digest = %q, want %q", entry.Digest, tt.wantDigest)
			}

			got, ok := New(dir, testBudget).Get("cv-edge")
			if !ok || !bytes.Equal(got, tt.wantValues) {
				t.Fatalf("get = (%q, %v), want (%q, true)", got, ok, tt.wantValues)
			}
		})
	}
}

func TestPutUnlessAsksTheGuardWithTheRenameAlreadyHeld(t *testing.T) {
	c := New(t.TempDir(), testBudget)

	const key = "chart"

	held := false
	refused := c.PutUnless(key, "old", []byte("old values\n"), func() bool {
		if c.mu.TryLock() {
			c.mu.Unlock()

			return true
		}
		held = true

		return false
	})

	if !held {
		t.Fatal("the guard ran without the write lock: another writer could rename between its answer and this one")
	}
	if !errors.Is(refused, ErrSuperseded) {
		t.Fatalf("PutUnless returned %v, want ErrSuperseded", refused)
	}
	if _, ok := c.Get(key); ok {
		t.Fatal("a refused entry was published")
	}

	if err := c.Put(key, "new", []byte("new values\n")); err != nil {
		t.Fatalf("Put after a refusal: %v", err)
	}

	c.mu.Lock()
	reserved, used := c.reserved, c.used
	c.mu.Unlock()

	if reserved != 0 {
		t.Fatalf("reserved is %d after both writes settled, want 0", reserved)
	}
	if want := onDisk(int64(len(encodeEntry("new", []byte("new values\n"))))); used != want {
		t.Fatalf("used is %d, want %d: only the published entry counts", used, want)
	}
}
