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
	"sync"
	"testing"
)

func TestPutGetRoundTrip(t *testing.T) {
	c := New(t.TempDir())

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
	c := New(t.TempDir())

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
