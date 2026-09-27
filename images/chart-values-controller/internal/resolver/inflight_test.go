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

package resolver

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// waiterBudget is the budget a waiter is given in the deadline test. It is the
// thing under test there — how long the waiter is willing to wait — and not a
// window for another goroutine to reach some point in the code.
const waiterBudget = 50 * time.Millisecond

// TestInflightCollapsesConcurrentCallersOnOneKeyIntoOneCall pins the behaviour the
// polling API needs: N consoles asking for the same chart at the same moment all
// miss the cache, and must still cause a single download and extraction of the one
// artifact their shared key names.
func TestInflightCollapsesConcurrentCallersOnOneKeyIntoOneCall(t *testing.T) {
	const callers = 8

	var g inflight
	var calls atomic.Int64

	release := make(chan struct{})

	// Every caller but the one that wins the key joins the call it is running, and
	// each of them reports having joined from inside do, at the point where it can
	// no longer run a call of its own. Only once all of them have is the leader
	// released, so the count below measures coalescing and not which goroutine the
	// scheduler happened to run first.
	var joined sync.WaitGroup
	joined.Add(callers - 1)
	g.onJoin = joined.Done

	type outcome struct {
		values []byte
		shared bool
		err    error
	}
	outcomes := make([]outcome, callers)

	var wg sync.WaitGroup
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()

			values, shared, err := g.do(context.Background(), "chart-key", func() ([]byte, error) {
				calls.Add(1)
				// Whoever leads stays in flight until every other caller has
				// joined, so the test measures coalescing rather than a race the
				// leader happened to win alone.
				<-release

				return []byte("replicaCount: 1\n"), nil
			})
			outcomes[i] = outcome{values: values, shared: shared, err: err}
		}()
	}

	joined.Wait()
	close(release)
	wg.Wait()

	if got := calls.Load(); got != 1 {
		t.Fatalf("the shared work ran %d times, want exactly 1: concurrent misses on one key must not each download the artifact", got)
	}

	leaders := 0
	for i, got := range outcomes {
		if got.err != nil {
			t.Fatalf("caller %d failed: %v", i, got.err)
		}
		if !bytes.Equal(got.values, []byte("replicaCount: 1\n")) {
			t.Fatalf("caller %d got %q, want the values of the single call", i, got.values)
		}
		if !got.shared {
			leaders++
		}
	}
	if leaders != 1 {
		t.Fatalf("%d callers reported running the call themselves, want 1", leaders)
	}
}

// TestInflightRunsCallsOnDifferentKeysConcurrently guards the other half: the group
// must serialize nothing beyond one key. Charts of two repositories — or, since the
// key carries the repository's identity, two repositories sharing a name — are
// separate artifacts, and one slow download must not hold the other's request.
func TestInflightRunsCallsOnDifferentKeysConcurrently(t *testing.T) {
	var g inflight

	started := map[string]chan struct{}{
		"first-key":  make(chan struct{}),
		"second-key": make(chan struct{}),
	}

	// Each call waits for the other to have started, so neither can finish unless
	// both are in flight at once: a group that blocked across keys would deadlock
	// here and the test would fail on its own deadline rather than hang.
	done := make(chan error, len(started))
	for key, start := range started {
		go func() {
			other := "first-key"
			if key == "first-key" {
				other = "second-key"
			}

			_, _, err := g.do(context.Background(), key, func() ([]byte, error) {
				close(start)
				select {
				case <-started[other]:
					return []byte(key), nil
				case <-time.After(5 * time.Second):
					return nil, errors.New("the call on the other key never started")
				}
			})
			done <- err
		}()
	}

	for range started {
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("a call on an independent key was blocked: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("calls on independent keys did not both complete")
		}
	}
}

// TestInflightForgetsACallThatProducedNothing covers the leak: whatever ends a
// call — a returned error, a panic, or a goroutine killed outright — the key must
// be free again, or one failed download would leave every later request for that
// chart waiting on a call that will never finish.
func TestInflightForgetsACallThatProducedNothing(t *testing.T) {
	failure := errors.New("fetching the artifact failed")

	tests := []struct {
		name    string
		run     func()
		wantErr error
	}{
		{
			// The shared call ends by returning failure, which every waiter is
			// handed as its own error.
			name:    "a call that returned an error",
			run:     func() {},
			wantErr: failure,
		},
		{
			name: "a call that panicked",
			run: func() {
				panic("extraction blew up")
			},
			wantErr: errCallAborted,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var g inflight
			var calls atomic.Int64

			release := make(chan struct{})
			entered := make(chan struct{})
			waiterErr := make(chan error, 1)

			// The waiter reports from inside do, once it has found the call in
			// flight and can no longer start one of its own. The leader is held
			// until then, so the failure the waiter is meant to inherit cannot
			// happen before it is there to inherit it.
			joined := make(chan struct{})
			g.onJoin = func() { close(joined) }

			var wg sync.WaitGroup
			wg.Add(1)
			go func() {
				defer wg.Done()
				// A panic belongs to the request that caused it and travels no
				// further, so it is caught here rather than taking the test down.
				defer func() { _ = recover() }()

				_, _, _ = g.do(context.Background(), "chart-key", func() ([]byte, error) {
					calls.Add(1)
					close(entered)
					<-release
					test.run()

					return nil, failure
				})
			}()

			<-entered

			wg.Add(1)
			go func() {
				defer wg.Done()

				_, _, err := g.do(context.Background(), "chart-key", func() ([]byte, error) {
					return nil, errors.New("the waiter must not run its own call")
				})
				waiterErr <- err
			}()

			<-joined
			close(release)

			if err := <-waiterErr; !errors.Is(err, test.wantErr) {
				t.Fatalf("the waiter got %v, want %v", err, test.wantErr)
			}
			wg.Wait()

			if got := calls.Load(); got != 1 {
				t.Fatalf("the shared work ran %d times while one call was in flight, want 1", got)
			}

			// The entry must be gone, so the next request starts a fresh call
			// instead of joining a dead one.
			values, shared, err := g.do(context.Background(), "chart-key", func() ([]byte, error) {
				return []byte("replicaCount: 1\n"), nil
			})
			if err != nil {
				t.Fatalf("the call after the failure returned %v", err)
			}
			if shared {
				t.Fatal("the call after the failure joined the dead one instead of running")
			}
			if !bytes.Equal(values, []byte("replicaCount: 1\n")) {
				t.Fatalf("the call after the failure returned %q", values)
			}

			g.mu.Lock()
			left := len(g.calls)
			g.mu.Unlock()
			if left != 0 {
				t.Fatalf("%d in-flight entries left behind, want none", left)
			}
		})
	}
}

// TestInflightWaiterEndsOnItsOwnDeadline pins that joining a call never outlives the
// request that joined it: the handler puts a budget on every request, and a waiter
// that ignored it would hold its connection for as long as somebody else's download
// takes.
func TestInflightWaiterEndsOnItsOwnDeadline(t *testing.T) {
	tests := []struct {
		// ctx builds the waiter's context and, with it, what ends that context.
		// The second return value is run at the moment the waiter has joined the
		// call in flight: cancelling from there ends a request that is provably
		// already waiting, which a timer could only approximate. A case that needs
		// nothing done at that moment returns nil.
		name    string
		ctx     func(t *testing.T) (context.Context, func())
		wantErr error
	}{
		{
			name: "a cancelled request",
			ctx: func(t *testing.T) (context.Context, func()) {
				t.Helper()
				ctx, cancel := context.WithCancel(context.Background())
				t.Cleanup(cancel)

				return ctx, cancel
			},
			wantErr: context.Canceled,
		},
		{
			// The budget runs out on its own while the waiter waits; nothing has
			// to be timed against it.
			name: "a request out of budget",
			ctx: func(t *testing.T) (context.Context, func()) {
				t.Helper()
				ctx, cancel := context.WithTimeout(context.Background(), waiterBudget)
				t.Cleanup(cancel)

				return ctx, nil
			},
			wantErr: context.DeadlineExceeded,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var g inflight

			ctx, onJoin := test.ctx(t)
			g.onJoin = onJoin

			// The leader is held for far longer than the waiter's budget and is
			// released only when the test is over, so a waiter that waited for it
			// would fail on the test's own deadline.
			release := make(chan struct{})
			t.Cleanup(func() { close(release) })

			entered := make(chan struct{})
			leaderDone := make(chan struct{})
			go func() {
				defer close(leaderDone)

				_, _, _ = g.do(context.Background(), "chart-key", func() ([]byte, error) {
					close(entered)
					<-release

					return []byte("replicaCount: 1\n"), nil
				})
			}()

			<-entered

			values, shared, err := g.do(ctx, "chart-key", func() ([]byte, error) {
				return nil, errors.New("the waiter must not run its own call")
			})
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("the waiter returned %v, want %v", err, test.wantErr)
			}
			if !shared {
				t.Fatal("the waiter must report that it joined somebody else's call")
			}
			if values != nil {
				t.Fatalf("the waiter returned values %q from a call it never saw finish", values)
			}

			// The leader is untouched by the waiter giving up: it keeps running for
			// whoever else is waiting on the same artifact.
			select {
			case <-leaderDone:
				t.Fatal("the leader ended when the waiter gave up")
			default:
			}
		})
	}
}
