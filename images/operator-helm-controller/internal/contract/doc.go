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

// Package contract declares the contracts through which the collaborators and the
// reconcilers see a user-facing resource without knowing its kind.
//
// A Release is any of the release kinds of the module; a Repository is any of the
// repository kinds; a Catalog writes the chart catalog kind that mirrors one
// repository family. The implementations live in internal/adapter and
// internal/catalog.
//
// Two rules keep this package a leaf. It depends on nothing that depends on it —
// the API types, the status manager and the repository client — so the adapters,
// the catalogs and every collaborator package (internalobj, rbac, claim, namespace,
// reposync) can import it without a cycle.
// And a contract lives here rather than next to the package that consumes it
// whenever the consumer's own tests build an adapter for it: the adapters would
// then import the consumer to reach the interface, while the consumer's tests
// import the adapters.
//
// A collaborator only the reconcilers call is not such a case, and is declared
// where it is consumed — in the collaborators.go of each reconciler package.
package contract
