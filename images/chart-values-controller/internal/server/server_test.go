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

	"github.com/deckhouse/chart-values-controller/internal/resolver"
)

type fakeResolver struct {
	result resolver.Result
	err    error
}

func (f fakeResolver) Resolve(_ context.Context, _ resolver.Request) (resolver.Result, error) {
	return f.result, f.err
}

func do(t *testing.T, res chartValuesResolver, body string) *httptest.ResponseRecorder {
	t.Helper()

	srv := New("", res)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chart-values", strings.NewReader(body))
	srv.handleChartValues(rec, req)

	return rec
}

const validBody = `{"helmClusterAddonChartName":"podinfo","helmClusterAddonRepository":"github","chartVersion":"6.7.1"}`

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
		{resolver.OutcomeValuesNotFound, http.StatusUnprocessableEntity, "VALUES_NOT_FOUND"},
		{resolver.OutcomeFetchFailed, http.StatusBadGateway, "CHART_FETCH_FAILED"},
	}

	for _, c := range cases {
		rec := do(t, fakeResolver{result: resolver.Result{Outcome: c.outcome, Message: "detail"}}, validBody)
		if rec.Code != c.wantStatus {
			t.Fatalf("outcome %s: status = %d, want %d", c.outcome, rec.Code, c.wantStatus)
		}
		assertErrorCode(t, rec.Body.Bytes(), c.wantCode)
	}
}

func TestHandleInvalidJSON(t *testing.T) {
	rec := do(t, fakeResolver{}, "not-json")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	assertErrorCode(t, rec.Body.Bytes(), "INVALID_REQUEST")
}

func TestHandleMissingFields(t *testing.T) {
	rec := do(t, fakeResolver{}, `{"helmClusterAddonChartName":"podinfo"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	assertErrorCode(t, rec.Body.Bytes(), "INVALID_REQUEST")
}

func TestHandleInternalError(t *testing.T) {
	rec := do(t, fakeResolver{err: context.DeadlineExceeded}, validBody)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	assertErrorCode(t, rec.Body.Bytes(), "INTERNAL")
}

func assertErrorCode(t *testing.T, body []byte, want string) {
	t.Helper()

	var resp struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Error.Code != want {
		t.Fatalf("error code = %q, want %q", resp.Error.Code, want)
	}
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
