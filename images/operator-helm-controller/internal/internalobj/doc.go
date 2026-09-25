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

// Package internalobj is the gateway to the internal flux objects the module
// creates in its own namespace: the HelmChart or OCIRepository a release is served
// from, the HelmRelease helm-controller acts on, the HelmRepository a repository
// that hands out packaged archives needs, and the reconcile requests that prod
// their controllers.
//
// Every write to a flux type goes through here. A reconciler asks for an object to
// be brought to a state and is handed an outcome describing what the object then
// reported; it never constructs or fills in a sourcev1 or helmv2 object itself. The
// few flux types that do reach a reconciler's vocabulary are the objects a cleanup
// hands back, which it only checks for presence and passes to the deletion wait.
//
// The outcome types live here rather than with the reconciler that reads them
// because they are what this package produces: the reconciler's Evaluate turns them
// into conditions, and two reconcilers read the same ones.
package internalobj
