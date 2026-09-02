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
	"errors"
	"fmt"
	"net/http"

	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
)

// TerminalError marks a repository failure that will not resolve by retrying:
// the remote rejected the request or the configuration is invalid. Callers use
// it to move the repository to Stalled instead of scheduling another attempt at
// the normal cadence.
type TerminalError struct {
	Reason  string
	Message string
	Err     error
}

func (e *TerminalError) Error() string {
	if e.Err == nil {
		return e.Message
	}

	return fmt.Sprintf("%s: %s", e.Message, e.Err)
}

func (e *TerminalError) Unwrap() error { return e.Err }

// AsTerminal reports whether err wraps a TerminalError and returns it.
func AsTerminal(err error) (*TerminalError, bool) {
	var terminal *TerminalError
	if errors.As(err, &terminal) {
		return terminal, true
	}

	return nil, false
}

// TerminalFromStatusCode maps a rejection status code to a terminal error and
// returns nil for codes that are worth retrying.
func TerminalFromStatusCode(code int, url string) *TerminalError {
	switch {
	case code == http.StatusUnauthorized, code == http.StatusForbidden:
		return &TerminalError{
			Reason:  helmv1alpha1.ReasonAuthenticationFailed,
			Message: fmt.Sprintf("repository %s rejected the credentials (HTTP %d)", url, code),
		}
	case code == http.StatusNotFound:
		return &TerminalError{
			Reason:  helmv1alpha1.ReasonSourceNotFound,
			Message: fmt.Sprintf("repository %s not found (HTTP %d)", url, code),
		}
	case code >= 400 && code < 500:
		return &TerminalError{
			Reason:  helmv1alpha1.ReasonSourceRejectedRequest,
			Message: fmt.Sprintf("repository %s rejected the request (HTTP %d)", url, code),
		}
	default:
		return nil
	}
}
