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

const waiterBudget = 50 * time.Millisecond

// joinSignal reports that a caller of do is waiting on its call: do uses the context for nothing else.
type joinSignal struct {
	context.Context
	joined func()
}

func (c joinSignal) Done() <-chan struct{} {
	c.joined()

	return c.Context.Done()
}

func TestInflightCollapsesConcurrentCallersOnOneKeyIntoOneCall(t *testing.T) {
	const callers = 8

	var g inflight
	var calls atomic.Int64

	release := make(chan struct{})

	entered := make(chan struct{})

	type outcome struct {
		values []byte
		err    error
	}
	outcomes := make([]outcome, callers)

	var joined sync.WaitGroup
	joined.Add(callers - 1)

	var wg sync.WaitGroup
	for i := range callers {
		ctx := context.Background()
		if i > 0 {
			<-entered

			var once sync.Once
			ctx = joinSignal{Context: ctx, joined: func() { once.Do(joined.Done) }}
		}

		wg.Add(1)
		go func() {
			defer wg.Done()

			values, err := g.do(ctx, "chart-key", func() ([]byte, error) {
				calls.Add(1)
				close(entered)
				<-release

				return []byte("replicaCount: 1\n"), nil
			})
			outcomes[i] = outcome{values: values, err: err}
		}()
	}

	joined.Wait()
	close(release)
	wg.Wait()

	if got := calls.Load(); got != 1 {
		t.Fatalf("the shared work ran %d times, want exactly 1: concurrent misses on one key must not each download the artifact", got)
	}

	for i, got := range outcomes {
		if got.err != nil {
			t.Fatalf("caller %d failed: %v", i, got.err)
		}
		if !bytes.Equal(got.values, []byte("replicaCount: 1\n")) {
			t.Fatalf("caller %d got %q, want the values of the single call", i, got.values)
		}
	}
}

func TestInflightRunsCallsOnDifferentKeysConcurrently(t *testing.T) {
	var g inflight

	started := map[string]chan struct{}{
		"first-key":  make(chan struct{}),
		"second-key": make(chan struct{}),
	}

	done := make(chan error, len(started))
	for key, start := range started {
		go func() {
			other := "first-key"
			if key == "first-key" {
				other = "second-key"
			}

			_, err := g.do(context.Background(), key, func() ([]byte, error) {
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

func TestInflightForgetsACallThatProducedNothing(t *testing.T) {
	failure := errors.New("fetching the artifact failed")

	tests := []struct {
		name    string
		run     func()
		wantErr error
	}{
		{
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

			joined := make(chan struct{})
			var joinOnce sync.Once
			waiterCtx := joinSignal{Context: context.Background(), joined: func() { joinOnce.Do(func() { close(joined) }) }}

			var wg sync.WaitGroup
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer func() { _ = recover() }()

				_, _ = g.do(context.Background(), "chart-key", func() ([]byte, error) {
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

				_, err := g.do(waiterCtx, "chart-key", func() ([]byte, error) {
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

			values, err := g.do(context.Background(), "chart-key", func() ([]byte, error) {
				calls.Add(1)

				return []byte("replicaCount: 1\n"), nil
			})
			if err != nil {
				t.Fatalf("the call after the failure returned %v", err)
			}
			if got := calls.Load(); got != 2 {
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

func TestInflightWaiterEndsOnItsOwnDeadline(t *testing.T) {
	tests := []struct {
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

			parent, onJoin := test.ctx(t)
			var joinOnce sync.Once
			ctx := joinSignal{Context: parent, joined: func() {
				if onJoin != nil {
					joinOnce.Do(onJoin)
				}
			}}

			release := make(chan struct{})
			t.Cleanup(func() { close(release) })

			entered := make(chan struct{})
			leaderDone := make(chan struct{})
			go func() {
				defer close(leaderDone)

				_, _ = g.do(context.Background(), "chart-key", func() ([]byte, error) {
					close(entered)
					<-release

					return []byte("replicaCount: 1\n"), nil
				})
			}()

			<-entered

			values, err := g.do(ctx, "chart-key", func() ([]byte, error) {
				return nil, errors.New("the waiter must not run its own call")
			})
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("the waiter returned %v, want %v", err, test.wantErr)
			}
			if values != nil {
				t.Fatalf("the waiter returned values %q from a call it never saw finish", values)
			}

			select {
			case <-leaderDone:
				t.Fatal("the leader ended when the waiter gave up")
			default:
			}
		})
	}
}
