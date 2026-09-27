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

// Package namespace creates the target namespace of a release that is allowed to
// bring its own, and never modifies one that already exists: the namespace belongs
// to whoever created it, and an addon that happens to install into a namespace the
// cluster already uses must not relabel it.
//
// Today only the addon controller wires the creating implementation in. Other
// families deploy into a namespace that must already exist and get ExistingTarget,
// which creates nothing. Both answer the second question this package owns —
// whether the target namespace is terminating — because a release whose namespace
// is going away is torn down differently from one that is merely deleted.
package namespace
