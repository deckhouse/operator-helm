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

package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/deckhouse/chart-values-controller/internal/auth"
	"github.com/deckhouse/chart-values-controller/internal/resolver"
)

type fakeResolver struct {
	result resolver.Result
	err    error
}

func (f fakeResolver) Resolve(_ context.Context, _ resolver.Request) (resolver.Result, error) {
	return f.result, f.err
}

type fakeReviewer struct {
	result auth.Result
	err    error
}

func (f fakeReviewer) Review(_ context.Context, _ string, _ auth.Access) (auth.Result, error) {
	return f.result, f.err
}

// authorized is the default reviewer for tests unconcerned with authorization.
var authorized = fakeReviewer{result: auth.Result{Authenticated: true, Authorized: true}}

func do(t *testing.T, res chartValuesResolver, body string) *httptest.ResponseRecorder {
	t.Helper()

	return doAuth(t, res, authorized, body)
}

func doAuth(t *testing.T, res chartValuesResolver, rev tokenReviewer, body string) *httptest.ResponseRecorder {
	t.Helper()

	srv := New("", res, rev, NewOptions{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chart-values", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-token")
	srv.handleChartValues(rec, req)

	return rec
}

const validBody = `{"repositoryKind":"HelmClusterAddonRepository","repositoryName":"github","chart":"podinfo","version":"6.7.1"}`

func TestHandleReady(t *testing.T) {
	rec := do(t, fakeResolver{result: resolver.Result{Outcome: resolver.OutcomeReady, Values: []byte("a: b\n")}}, validBody)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var resp struct {
		Data map[string]string `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Data["values.yaml"] != "a: b\n" {
		t.Fatalf("values.yaml = %q", resp.Data["values.yaml"])
	}
}

func TestHandlePending(t *testing.T) {
	rec := do(t, fakeResolver{result: resolver.Result{Outcome: resolver.OutcomePending}}, validBody)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Fatal("missing Retry-After header")
	}
	assertCode(t, rec.Body.Bytes(), "code", "CHART_PENDING")
}

func TestHandleOutcomeStatusCodes(t *testing.T) {
	cases := []struct {
		outcome    resolver.Outcome
		wantStatus int
		wantCode   string
	}{
		{resolver.OutcomeRepositoryNotFound, http.StatusNotFound, "REPOSITORY_NOT_FOUND"},
		{resolver.OutcomeUnsupportedRepositoryKind, http.StatusBadRequest, "UNSUPPORTED_REPOSITORY_KIND"},
		{resolver.OutcomeValuesNotFound, http.StatusUnprocessableEntity, "VALUES_NOT_FOUND"},
		{resolver.OutcomeFetchFailed, http.StatusBadGateway, "CHART_FETCH_FAILED"},
	}

	for _, c := range cases {
		rec := do(t, fakeResolver{result: resolver.Result{Outcome: c.outcome, Message: "detail"}}, validBody)
		if rec.Code != c.wantStatus {
			t.Fatalf("outcome %s: status = %d, want %d", c.outcome, rec.Code, c.wantStatus)
		}
		assertCode(t, rec.Body.Bytes(), "code", c.wantCode)
	}
}

func TestHandleInvalidJSON(t *testing.T) {
	rec := do(t, fakeResolver{}, "not-json")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	assertCode(t, rec.Body.Bytes(), "code", "INVALID_REQUEST")
}

func TestHandleMissingFields(t *testing.T) {
	rec := do(t, fakeResolver{}, `{"repositoryName":"github","chart":"podinfo"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	assertCode(t, rec.Body.Bytes(), "code", "INVALID_REQUEST")
}

func TestHandleInternalError(t *testing.T) {
	rec := do(t, fakeResolver{err: context.DeadlineExceeded}, validBody)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	assertCode(t, rec.Body.Bytes(), "code", "INTERNAL")
}

func TestHandleMissingToken(t *testing.T) {
	srv := New("", fakeResolver{}, authorized, NewOptions{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chart-values", strings.NewReader(validBody))
	srv.handleChartValues(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	assertCode(t, rec.Body.Bytes(), "code", "UNAUTHENTICATED")
}

func TestHandleUnauthenticated(t *testing.T) {
	rec := doAuth(t, fakeResolver{}, fakeReviewer{result: auth.Result{Authenticated: false}}, validBody)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	assertCode(t, rec.Body.Bytes(), "code", "UNAUTHENTICATED")
}

func TestHandleForbidden(t *testing.T) {
	rec := doAuth(t, fakeResolver{}, fakeReviewer{result: auth.Result{Authenticated: true, Authorized: false}}, validBody)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	assertCode(t, rec.Body.Bytes(), "code", "FORBIDDEN")
}

func TestHandleReviewError(t *testing.T) {
	rec := doAuth(t, fakeResolver{}, fakeReviewer{err: context.DeadlineExceeded}, validBody)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	assertCode(t, rec.Body.Bytes(), "code", "INTERNAL")
}

func assertCode(t *testing.T, body []byte, field, want string) {
	t.Helper()

	var resp map[string]any
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp[field] != want {
		t.Fatalf("%s = %v, want %q", field, resp[field], want)
	}
}
