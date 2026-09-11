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

type recordingReviewer struct {
	result auth.Result
	err    error
	access auth.Access
}

func (f *recordingReviewer) Review(_ context.Context, _ string, access auth.Access) (auth.Result, error) {
	f.access = access

	return f.result, f.err
}

type recordingResolver struct {
	result resolver.Result
	err    error
	req    resolver.Request
}

func (f *recordingResolver) Resolve(_ context.Context, req resolver.Request) (resolver.Result, error) {
	f.req = req

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

// TestHandleRejectsANamespacedKindWithoutANamespace covers the request-shape guard
// at the HTTP boundary: the resolver would refuse it too, but the client deserves a
// 400 naming the missing field rather than a generic outcome.
func TestHandleRejectsANamespacedKindWithoutANamespace(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{
			name: "the field is absent",
			body: `{"repositoryKind":"HelmApplicationRepository","repositoryName":"stable","chart":"podinfo","version":"6.7.1"}`,
		},
		{
			// Without this, a namespace no cluster can have reaches the access
			// review and comes back as a 403 the caller cannot act on.
			name: "the field holds a name no namespace can have",
			body: `{"repositoryKind":"HelmApplicationRepository","namespace":"  ","repositoryName":"stable","chart":"podinfo","version":"6.7.1"}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := do(t, fakeResolver{}, tc.body)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", rec.Code)
			}
			assertCode(t, rec.Body.Bytes(), "code", "INVALID_REQUEST")
		})
	}
}

// TestHandleAuthorizesPerFamily pins which permission each repository kind demands:
// the addon family a cluster-scoped create of HelmClusterAddon, both application
// kinds a create of HelmApplication in the request's namespace — that is the
// resource whose values are exposed by the answer.
func TestHandleAuthorizesPerFamily(t *testing.T) {
	cases := []struct {
		name          string
		body          string
		wantResource  string
		wantNamespace string
	}{
		{
			name:         "addon",
			body:         validBody,
			wantResource: "helmclusteraddons",
		},
		{
			name:          "namespaced application repository",
			body:          `{"repositoryKind":"HelmApplicationRepository","namespace":"team-a","repositoryName":"stable","chart":"podinfo","version":"6.7.1"}`,
			wantResource:  "helmapplications",
			wantNamespace: "team-a",
		},
		{
			name:          "cluster application repository",
			body:          `{"repositoryKind":"HelmClusterApplicationRepository","namespace":"team-a","repositoryName":"shared","chart":"podinfo","version":"6.7.1"}`,
			wantResource:  "helmapplications",
			wantNamespace: "team-a",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rev := &recordingReviewer{result: auth.Result{Authenticated: true, Authorized: true}}
			rec := doAuth(t, fakeResolver{result: resolver.Result{Outcome: resolver.OutcomeReady}}, rev, tc.body)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
			}
			if rev.access.Resource != tc.wantResource || rev.access.Namespace != tc.wantNamespace || rev.access.Verb != "create" {
				t.Fatalf("access = %+v, want create on %s in %q", rev.access, tc.wantResource, tc.wantNamespace)
			}
		})
	}
}

// TestHandlePassesTheNamespaceToTheResolver makes sure the namespace is not merely
// validated and dropped.
func TestHandlePassesTheNamespaceToTheResolver(t *testing.T) {
	res := &recordingResolver{result: resolver.Result{Outcome: resolver.OutcomeReady}}
	rec := doAuth(t, res, authorized, `{"repositoryKind":"HelmApplicationRepository","namespace":"team-a","repositoryName":"stable","chart":"podinfo","version":"6.7.1"}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if res.req.Namespace != "team-a" || res.req.Kind != resolver.RepositoryKindHelmApplication {
		t.Fatalf("request = %+v, want the namespace and the lower-cased kind", res.req)
	}
}

// TestHandleInvalidRequestOutcome maps the resolver's request-shape refusal.
func TestHandleInvalidRequestOutcome(t *testing.T) {
	rec := do(t, fakeResolver{result: resolver.Result{Outcome: resolver.OutcomeInvalidRequest, Message: "detail"}}, validBody)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	assertCode(t, rec.Body.Bytes(), "code", "INVALID_REQUEST")
}

// TestHandleUnknownRepositoryKind pins the response contract for a kind the server
// does not recognise: it must be reported as UNSUPPORTED_REPOSITORY_KIND, the same
// code the resolver's own outcome of that name maps to, not a generic INVALID_REQUEST.
func TestHandleUnknownRepositoryKind(t *testing.T) {
	rec := do(t, fakeResolver{}, `{"repositoryKind":"SomethingElse","repositoryName":"github","chart":"podinfo","version":"6.7.1"}`)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	assertCode(t, rec.Body.Bytes(), "code", "UNSUPPORTED_REPOSITORY_KIND")
	assertCode(t, rec.Body.Bytes(), "error", `unsupported repository kind "SomethingElse"`)
}

// TestHandleForbiddenMessageNamesTheResourceKind pins the FORBIDDEN message's
// wording: it names the Kubernetes kind the caller may not create (e.g.
// "HelmClusterAddon"), not the lower-cased plural resource string used in the
// SubjectAccessReview.
func TestHandleForbiddenMessageNamesTheResourceKind(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		wantMsg string
	}{
		{"addon", validBody, "not allowed to create HelmClusterAddon"},
		{
			"namespaced application repository",
			`{"repositoryKind":"HelmApplicationRepository","namespace":"team-a","repositoryName":"stable","chart":"podinfo","version":"6.7.1"}`,
			"not allowed to create HelmApplication",
		},
		{
			"cluster application repository",
			`{"repositoryKind":"HelmClusterApplicationRepository","namespace":"team-a","repositoryName":"shared","chart":"podinfo","version":"6.7.1"}`,
			"not allowed to create HelmApplication",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := doAuth(t, fakeResolver{}, fakeReviewer{result: auth.Result{Authenticated: true, Authorized: false}}, tc.body)

			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403", rec.Code)
			}
			assertCode(t, rec.Body.Bytes(), "code", "FORBIDDEN")
			assertCode(t, rec.Body.Bytes(), "error", tc.wantMsg)
		})
	}
}
