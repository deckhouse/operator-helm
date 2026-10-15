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

var errCallAborted = errors.New("the in-flight call did not complete")

type inflight struct {
	mu    sync.Mutex
	calls map[string]*inflightCall
}

type inflightCall struct {
	// done is closed once the result fields are final.
	done     chan struct{}
	values   []byte
	err      error
	panicked any
}

func (g *inflight) do(ctx context.Context, key string, fn func() ([]byte, error)) ([]byte, error) {
	g.mu.Lock()
	call, ok := g.calls[key]
	if !ok {
		if g.calls == nil {
			g.calls = make(map[string]*inflightCall)
		}
		call = &inflightCall{done: make(chan struct{})}
		g.calls[key] = call

		go func() {
			completed := false
			defer func() {
				if !completed {
					call.err = errCallAborted
					call.panicked = recover()
				}

				g.mu.Lock()
				delete(g.calls, key)
				g.mu.Unlock()

				close(call.done)
			}()

			call.values, call.err = fn()
			completed = true
		}()
	}
	g.mu.Unlock()

	select {
	case <-call.done:
		// Only the caller that started the call re-raises its panic, as if it had run fn itself.
		if !ok && call.panicked != nil {
			panic(call.panicked)
		}

		return call.values, call.err
	case <-ctx.Done():
		// A caller's cancellation ends its own wait; the call keeps running for the others.
		return nil, ctx.Err()
	}
}
