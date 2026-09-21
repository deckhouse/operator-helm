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

import (
	"net/http"
	"testing"

	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
)

// TestTerminalFromStatusCode pins which rejections are verdicts and which are
// "later". The distinction decides whether a repository is reported as Stalled and
// whether a release that could not examine its artifact ever tries again, so the one
// retriable code in the 4xx range is pinned alongside the terminal ones.
func TestTerminalFromStatusCode(t *testing.T) {
	tests := []struct {
		code   int
		reason string
	}{
		{http.StatusUnauthorized, helmv1alpha1.ReasonAuthenticationFailed},
		{http.StatusForbidden, helmv1alpha1.ReasonAuthenticationFailed},
		{http.StatusNotFound, helmv1alpha1.ReasonSourceNotFound},
		{http.StatusBadRequest, helmv1alpha1.ReasonSourceRejectedRequest},
		{http.StatusTooManyRequests, ""},
		{http.StatusInternalServerError, ""},
		{http.StatusOK, ""},
	}

	for _, test := range tests {
		terminal := TerminalFromStatusCode(test.code, "https://charts.example.invalid")

		if test.reason == "" {
			if terminal != nil {
				t.Fatalf("HTTP %d = %+v, want it left retriable", test.code, terminal)
			}

			continue
		}

		if terminal == nil {
			t.Fatalf("HTTP %d was left retriable, want reason %q", test.code, test.reason)
		}
		if terminal.Reason != test.reason {
			t.Fatalf("HTTP %d reason = %q, want %q", test.code, terminal.Reason, test.reason)
		}
	}
}
