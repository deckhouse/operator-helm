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

// Package adapter binds each user-facing kind to the contracts of
// internal/source. Everything that differs between two kinds of the same role —
// labels, internal object names, the catalog kind, who consumes the charts —
// lives here and nowhere else; the services and the reconcilers stay kind-agnostic.
//
// An adapter wraps the API object rather than embedding it: the wrapper is not
// registered in the scheme, so it must never reach the client, and an explicit
// Object() accessor makes that boundary visible at every call site.
package adapter
