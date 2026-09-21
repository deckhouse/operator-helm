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

// Package release holds the one reconciler every release kind is served by:
// HelmClusterAddon and HelmApplication run the exact same pass, and a new family
// is added by wiring collaborators rather than by copying a control flow.
//
// What tells the families apart arrives as Deps: how the API object is read, how
// its repository and catalog are found, whether a repository/chart pair is
// claimed, whether the target namespace is created, and which identity the chart
// is applied with. The reconciler itself never names a kind.
//
// The API object is only ever reached through a source.Release adapter. The
// adapter is not a registered type, so it is never handed to the client; the
// object underneath it is reached through Object() at the few places that talk to
// the API server.
//
// Status is managed the way the repository package manages it, and for the same
// reason: a pass does cluster work and records what came of it in Inputs, and one
// deterministic function — Evaluate — turns that into the whole desired status,
// what to requeue and what to hand back to the work queue. No step writes a
// condition of its own, so what the release reports can be read in one place and
// tested without a cluster. The services this reconciler drives return outcomes
// rather than conditions for the same reason.
package release
