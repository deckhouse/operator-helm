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
	"context"
	"errors"
	"sync"
)

// errCallAborted is what a waiter is handed when the call it waited on ended
// without producing anything — it panicked, or its goroutine was killed. The
// panic itself belongs to the request that caused it and is not carried across:
// a waiter only needs to know that nothing came of the call, so that it retries
// instead of reading the zero value as an answer.
var errCallAborted = errors.New("the in-flight call did not complete")

// inflight collapses concurrent work sharing one key into a single call, with the
// other callers waiting for its result. /v1/chart-values is polled, so N consoles
// asking for the same chart at the same time arrive as N cache misses on the same
// key; without this each of them would download and unpack the very same artifact,
// multiplying network traffic, gzip CPU and peak memory by the number of requests
// in flight. One process serves this API (the Deployment is a single replica and
// takes no leader election), so an in-process group is the whole story — no
// distributed lock is involved.
//
// The zero value is ready for use.
type inflight struct {
	mu    sync.Mutex
	calls map[string]*inflightCall

	// onJoin, when set, is called once by every caller that has found a call for
	// its key already running and is therefore committed to waiting for it rather
	// than starting its own. It exists for tests, which have no other way to know
	// that a caller has passed that point — joining leaves no trace in the group —
	// and would otherwise have to guess with a sleep before releasing the leader.
	// Production never sets it, and an unset hook is not called at all, so it adds
	// nothing to the path a request takes.
	onJoin func()
}

type inflightCall struct {
	// done is closed once the result fields are final, which is the only ordering
	// a waiter needs: it reads them after receiving from the channel.
	done   chan struct{}
	values []byte
	err    error
}

// do runs fn for key unless a call for the same key is already running, in which
// case it waits for that call instead of starting its own. shared reports which of
// the two happened, because a shared failure is not this caller's verdict: it was
// produced under somebody else's context.
//
// A waiter never outlives its own context. When ctx ends first it returns that
// error, leaving the leader to finish for whoever else is waiting — the caller is
// free to answer "retry later", which is what this API already tells a polling
// client while an artifact is being prepared.
func (g *inflight) do(ctx context.Context, key string, fn func() ([]byte, error)) (values []byte, shared bool, err error) {
	g.mu.Lock()
	if call, ok := g.calls[key]; ok {
		g.mu.Unlock()

		if g.onJoin != nil {
			g.onJoin()
		}

		select {
		case <-call.done:
			return call.values, true, call.err
		case <-ctx.Done():
			return nil, true, ctx.Err()
		}
	}

	if g.calls == nil {
		g.calls = make(map[string]*inflightCall)
	}
	call := &inflightCall{done: make(chan struct{})}
	g.calls[key] = call
	g.mu.Unlock()

	completed := false
	// Dropping the entry from a defer is what keeps a key from being blocked
	// forever: an error, a cancelled context, a panic and a runtime.Goexit all take
	// this path, and the entry is gone before any waiter is released, so the next
	// request starts a fresh call rather than joining a dead one.
	defer func() {
		if !completed {
			call.err = errCallAborted
		}

		g.mu.Lock()
		delete(g.calls, key)
		g.mu.Unlock()

		close(call.done)
	}()

	call.values, call.err = fn()
	completed = true

	return call.values, false, call.err
}
