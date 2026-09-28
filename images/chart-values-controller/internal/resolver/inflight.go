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

// errCallAborted is what a waiter is handed when the call it waited on ended without producing anything.
var errCallAborted = errors.New("the in-flight call did not complete")

// inflight collapses concurrent work sharing one key into a single call, the other callers waiting for its result. The zero value is ready for use.
type inflight struct {
	mu    sync.Mutex
	calls map[string]*inflightCall

	// onJoin, when set, is called once by every caller that waits on a running call instead of starting its own; only tests set it.
	onJoin func()
}

type inflightCall struct {
	// done is closed once the result fields are final, which a waiter reads after receiving from it.
	done   chan struct{}
	values []byte
	err    error
}

// do runs fn for key unless a call for the same key is already running, in which case it waits for that call; shared reports which of the two happened.
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
	// A panic and a runtime.Goexit take this path too, and the entry is gone before any waiter is released.
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
