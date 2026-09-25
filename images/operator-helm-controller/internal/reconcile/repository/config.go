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

package repository

// ConfigFailure is a terminal configuration failure the reconciler recognizes
// before any attempt to reach the repository, such as an unsupported url scheme.
// Nothing else produces one, which is why it lives here rather than with the
// outcomes the synchronizer hands back.
type ConfigFailure struct {
	Reason  string
	Message string
	Err     error
}
