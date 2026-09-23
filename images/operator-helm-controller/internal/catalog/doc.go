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

// Package catalog writes the chart catalog of a repository into the chart
// catalog kind of its family. It is the one place in the controller that is
// generic over API types: the catalog objects must be created, listed and
// status-patched with their concrete kind, and everything a kind contributes —
// constructors, accessors, the naming function, who consumes the charts — arrives
// through a Config. The result is exposed as source.Catalog, so no generic type
// leaks into the services.
package catalog
