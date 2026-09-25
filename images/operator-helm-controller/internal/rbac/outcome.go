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

package rbac

// IdentityOutcome is the result of reconciling the identity a release is applied
// with. Terminal marks a failure that will not resolve by retrying: an object the
// identity needs already occupies its name and is not the operator's to touch.
// Reason and Message are what the release reports, filled in by the service so both
// classes of failure are named where they are recognized.
type IdentityOutcome struct {
	Err      error
	Terminal bool
	Reason   string
	Message  string
}
