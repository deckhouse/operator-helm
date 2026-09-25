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

// Package claim keeps two cluster addons off the same repository/chart pair.
//
// The claim is a Lease in the operator's namespace, named after the pair and not
// after the addon: the internal HelmChart both addons would create carries the same
// name, so without a claim the second one would quietly take the first one's chart
// over. The Lease is the only thing standing between them, which is why the holder
// is read through a direct API reader rather than the controller cache - a stale
// read here hands the pair to two owners at once.
//
// Only the cluster addon family needs this. An application is namespaced, and its
// internal objects are named after its namespace as well as its chart.
package claim
