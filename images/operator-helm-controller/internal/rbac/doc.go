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

// Package rbac owns the identity a release is applied with: the ServiceAccount
// helm-controller impersonates, the Role that grants it everything inside the
// target namespace, and the RoleBinding between them.
//
// Only the application families need one. An addon is applied by the controller's
// own identity, which is why the release reconciler takes this collaborator as an
// interface it can leave empty rather than a package it always calls.
//
// The package refuses to touch an object it does not own, and says so terminally:
// a ServiceAccount or a RoleBinding that already occupies the name and carries no
// managed-by label belongs to somebody else, and no retry will change that.
package rbac
