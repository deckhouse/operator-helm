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
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/google/go-containerregistry/pkg/v1/remote/transport"

	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
)

func TestFetchChartsOCIRejectsURLWithoutImageName(t *testing.T) {
	_, err := OCIRepositoryDefaultClient.FetchCharts(context.Background(), "oci://ghcr.io", nil)
	if err == nil {
		t.Fatal("expected error for url without an image name")
	}

	terminal, ok := AsTerminal(err)
	if !ok {
		t.Fatalf("expected terminal error, got %v", err)
	}
	if terminal.Reason != helmv1alpha1.ReasonInvalidRepositoryURL {
		t.Fatalf("expected reason %q, got %q", helmv1alpha1.ReasonInvalidRepositoryURL, terminal.Reason)
	}
}

func TestClassifyRemoteError(t *testing.T) {
	cases := []struct {
		name         string
		err          error
		wantTerminal bool
		wantReason   string
	}{
		{
			name:         "unauthorized is terminal",
			err:          &transport.Error{StatusCode: http.StatusUnauthorized},
			wantTerminal: true,
			wantReason:   helmv1alpha1.ReasonAuthenticationFailed,
		},
		{
			name:         "not found is terminal",
			err:          &transport.Error{StatusCode: http.StatusNotFound},
			wantTerminal: true,
			wantReason:   helmv1alpha1.ReasonSourceNotFound,
		},
		{
			name:         "server error is retriable",
			err:          &transport.Error{StatusCode: http.StatusBadGateway},
			wantTerminal: false,
		},
		{
			name:         "transport failure is retriable",
			err:          errors.New("dial tcp: connection refused"),
			wantTerminal: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyRemoteError(tc.err, "ghcr.io/example/chart")
			terminal, ok := AsTerminal(got)
			if ok != tc.wantTerminal {
				t.Fatalf("terminal=%v, want %v (err %v)", ok, tc.wantTerminal, got)
			}
			if tc.wantTerminal && terminal.Reason != tc.wantReason {
				t.Fatalf("expected reason %q, got %q", tc.wantReason, terminal.Reason)
			}
			if !errors.Is(got, tc.err) {
				t.Fatalf("classified error must wrap the original, got %v", got)
			}
		})
	}
}
