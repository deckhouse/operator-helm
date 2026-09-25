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

// Package claim enforces that at most one HelmClusterAddon uses a given
// repository/chart pair.
//
// The validating webhook cannot guarantee it: admission requests are served
// concurrently and before the object is persisted, so two simultaneous creates can
// both pass. Uniqueness is delegated instead to the API server's only atomic
// cross-object primitive — the uniqueness of an object name. Each pair maps to one
// Lease name in the operator's namespace; whoever creates that Lease first owns the
// pair, and every other addon reconciles into a conflict. The holder is read
// through a direct API reader rather than the controller cache, because an
// acquisition decision made against stale data hands the pair to two owners.
//
// It is addon-only in substance, not just by convention: the Lease name carries
// neither the release kind nor a namespace, and the holder recorded on it is read
// back as a HelmClusterAddon. Another family needing uniqueness would have to bring
// its own implementation of the release reconciler's Claims rather than reuse this
// one.
package claim
