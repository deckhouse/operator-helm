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

// Package source declares the contracts through which the services and the
// reconcilers see a user-facing resource without knowing its kind.
//
// A Repository is any of the three repository kinds of the module; a Catalog
// writes the chart catalog kind that mirrors one repository family. The
// implementations live in internal/adapter and internal/catalog. This package
// depends only on the API types and the status manager so that services,
// adapters and catalogs can all import it without a cycle — which is also why the
// contracts are not declared next to their consumer in internal/services: the
// services' own tests build adapters.
package source
