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
	"sync"
	"testing"
)

const testDigest = "sha256:0123456789abcdef"

func TestGetServesOnlyTheRevisionItHolds(t *testing.T) {
	c := New(1 << 20)
	want := []byte("replicaCount: 1\n")

	if err := c.Put("k", "sha256:a", want); err != nil {
		t.Fatalf("put: %v", err)
	}

	if got, ok := c.Get("k", "sha256:a"); !ok || !bytes.Equal(got, want) {
		t.Fatalf("get of the held revision = (%q, %v), want (%q, true)", got, ok, want)
	}
	if _, ok := c.Get("k", "sha256:b"); ok {
		t.Fatal("get of another revision must miss")
	}
	if _, ok := c.Get("other", "sha256:a"); ok {
		t.Fatal("get of another key must miss")
	}

	if err := c.Put("empty", "", want); err != nil {
		t.Fatalf("put with an empty digest: %v", err)
	}
	if _, ok := c.Get("empty", ""); ok {
		t.Fatal("an empty digest must never count as a hit")
	}
}

func TestPutRetainsOnlyTheBytesItCharges(t *testing.T) {
	c := New(1 << 20)

	if err := c.Put("k", testDigest, make([]byte, 64, 1<<20)); err != nil {
		t.Fatalf("put: %v", err)
	}

	if got, _ := c.Get("k", testDigest); cap(got) != len(got) {
		t.Fatalf("the entry retains %d bytes, want the %d it is charged", cap(got), len(got))
	}
}

func TestPutReplacesTheEntryAndChargesItOnce(t *testing.T) {
	c := New(25)
	newer := bytes.Repeat([]byte("b"), 20)

	if err := c.Put("k", "sha256:a", bytes.Repeat([]byte("a"), 10)); err != nil {
		t.Fatalf("first put: %v", err)
	}
	if err := c.Put("k", "sha256:b", newer); err != nil {
		t.Fatalf("replacing put: %v", err)
	}

	if got, ok := c.Get("k", "sha256:b"); !ok || !bytes.Equal(got, newer) {
		t.Fatalf("get of the replacing revision = (%q, %v), want (%q, true)", got, ok, newer)
	}
	if _, ok := c.Get("k", "sha256:a"); ok {
		t.Fatal("the replaced revision must miss")
	}

	if err := c.Put("other", "sha256:c", bytes.Repeat([]byte("c"), 6)); !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("put over the 20 of 25 bytes in use = %v, want ErrBudgetExhausted", err)
	}
}

func TestPutRefusesAnEntryThatDoesNotFitTheBudget(t *testing.T) {
	c := New(10)
	content := bytes.Repeat([]byte("x"), 6)

	if err := c.Put("a", testDigest, content); err != nil {
		t.Fatalf("first put: %v", err)
	}
	if err := c.Put("b", testDigest, content); !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("second put = %v, want ErrBudgetExhausted", err)
	}

	if got, ok := c.Get("a", testDigest); !ok || !bytes.Equal(got, content) {
		t.Fatal("the entry stored before the budget ran out must survive: nothing is evicted")
	}
	if _, ok := c.Get("b", testDigest); ok {
		t.Fatal("a refused put must not leave an entry behind")
	}
}

func TestDeleteGivesTheSpaceBackToTheBudget(t *testing.T) {
	c := New(10)
	content := bytes.Repeat([]byte("q"), 10)

	if err := c.Put("a", testDigest, content); err != nil {
		t.Fatalf("put: %v", err)
	}

	c.Delete("a")

	if err := c.Put("b", testDigest, content); err != nil {
		t.Fatalf("put after delete: %v", err)
	}

	c.Delete("missing")
}

func TestConcurrentPutGetDeleteOfOneKey(t *testing.T) {
	const (
		key    = "k"
		budget = 1 << 10
	)

	c := New(budget)
	content := bytes.Repeat([]byte("a: b\n"), 64)

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				_ = c.Put(key, testDigest, content)
				c.Get(key, testDigest)
				c.Delete(key)
			}
		}()
	}
	wg.Wait()

	c.Delete(key)

	if err := c.Put(key, testDigest, bytes.Repeat([]byte("x"), budget)); err != nil {
		t.Fatalf("put of the whole budget after the goroutines = %v, want nil: the accounting drifted", err)
	}
}
