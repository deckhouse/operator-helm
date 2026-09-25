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

// Package reposync reads a remote chart repository and writes what it found into
// the cluster: the catalog of chart versions the releases pick from, and the
// secrets the flux objects need to reach the same registry.
//
// It is the only place that talks to a registry, and the outcome types say so: a
// fetch failure is told apart from a catalog write failure, and a fetch that was
// never attempted is told apart from one that succeeded and found nothing. A
// reconciler that could not make that distinction would report a cluster read
// failure as a healthy, empty repository.
package reposync
