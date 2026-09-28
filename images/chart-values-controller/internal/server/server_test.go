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
	"time"

	"github.com/deckhouse/chart-values-controller/internal/auth"
	"github.com/deckhouse/chart-values-controller/internal/chartartifact"
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

type blockingResolver struct {
	result resolver.Result
	err    error
}

func (b blockingResolver) Resolve(ctx context.Context, _ resolver.Request) (resolver.Result, error) {
	<-ctx.Done()

	return b.result, b.err
}

type blockingReviewer struct{}

func (blockingReviewer) Review(ctx context.Context, _ string, _ auth.Access) (auth.Result, error) {
	<-ctx.Done()

	return auth.Result{}, ctx.Err()
}

func doBudgeted(t *testing.T, res chartValuesResolver, rev tokenReviewer, timeout time.Duration) *httptest.ResponseRecorder {
	t.Helper()

	srv := New("", res, rev, NewOptions{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chart-values", strings.NewReader(validBody))
	req.Header.Set("Authorization", "Bearer test-token")
	withRequestDeadline(timeout, srv.routes()).ServeHTTP(rec, req)

	return rec
}

func TestARequestOutOfBudgetIsAnswered(t *testing.T) {
	cases := []struct {
		name string
		res  chartValuesResolver
	}{
		{
			name: "the step reports the cancellation as an error",
			res:  blockingResolver{err: context.DeadlineExceeded},
		},
		{
			name: "the step reports the cancellation as a fetch failure",
			res:  blockingResolver{result: resolver.Result{Outcome: resolver.OutcomeFetchFailed, Message: "context deadline exceeded"}},
		},
		{
			name: "the step reports the cancellation as pending",
			res:  blockingResolver{result: resolver.Result{Outcome: resolver.OutcomePending}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := doBudgeted(t, tc.res, authorized, 20*time.Millisecond)

			if rec.Code != http.StatusGatewayTimeout {
				t.Fatalf("status = %d, want 504 (body %s)", rec.Code, rec.Body.String())
			}
			assertCode(t, rec.Body.Bytes(), "code", "REQUEST_TIMEOUT")
		})
	}
}

func TestTheBudgetCoversTheAccessReview(t *testing.T) {
	rec := doBudgeted(t, fakeResolver{}, blockingReviewer{}, 20*time.Millisecond)

	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want 504 (body %s)", rec.Code, rec.Body.String())
	}
	assertCode(t, rec.Body.Bytes(), "code", "REQUEST_TIMEOUT")
}

func TestTheBudgetReachesEveryStep(t *testing.T) {
	var (
		reviewed  time.Time
		resolved  time.Time
		hasReview bool
	)

	rev := deadlineReviewer{at: &reviewed, ok: &hasReview}
	res := &deadlineResolver{at: &resolved}

	rec := doBudgeted(t, res, rev, time.Minute)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}

	if !hasReview {
		t.Fatal("the access review was given a context without a deadline")
	}
	if resolved.IsZero() {
		t.Fatal("the resolver was given a context without a deadline")
	}
	if !resolved.Equal(reviewed) {
		t.Fatalf("the resolver's deadline %s differs from the review's %s: the budget was replaced mid-request", resolved, reviewed)
	}
	if remaining := time.Until(resolved); remaining <= 0 || remaining > time.Minute {
		t.Fatalf("deadline is %s from now, want within (0, 1m]", remaining)
	}
}

func TestTheServerServesUnderItsOwnBudget(t *testing.T) {
	var (
		resolved  time.Time
		reviewed  time.Time
		hasReview bool
	)

	srv := New("", &deadlineResolver{at: &resolved}, deadlineReviewer{at: &reviewed, ok: &hasReview}, NewOptions{})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chart-values", strings.NewReader(validBody))
	req.Header.Set("Authorization", "Bearer test-token")
	srv.handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if resolved.IsZero() {
		t.Fatal("the server served a request with no budget on it")
	}
	if remaining := time.Until(resolved); remaining <= 0 || remaining > requestTimeout {
		t.Fatalf("deadline is %s from now, want within (0, %s]", remaining, requestTimeout)
	}
}

type deadlineReviewer struct {
	at *time.Time
	ok *bool
}

func (d deadlineReviewer) Review(ctx context.Context, _ string, _ auth.Access) (auth.Result, error) {
	*d.at, *d.ok = ctx.Deadline()

	return auth.Result{Authenticated: true, Authorized: true}, nil
}

type deadlineResolver struct {
	at *time.Time
}

func (d *deadlineResolver) Resolve(ctx context.Context, _ resolver.Request) (resolver.Result, error) {
	*d.at, _ = ctx.Deadline()

	return resolver.Result{Outcome: resolver.OutcomeReady, Values: []byte("a: b\n")}, nil
}

func TestPendingIsStillAnsweredUnderTheBudget(t *testing.T) {
	rec := doBudgeted(t, fakeResolver{result: resolver.Result{Outcome: resolver.OutcomePending}}, authorized, time.Minute)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (body %s)", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Fatal("missing Retry-After header")
	}
	assertCode(t, rec.Body.Bytes(), "code", "CHART_PENDING")
}

func TestTheBudgetOutlastsTheStepsUnderIt(t *testing.T) {
	srv := newHTTPServer("", http.NewServeMux())

	if srv.WriteTimeout <= requestTimeout {
		t.Fatalf("WriteTimeout %s must outlast the request budget %s, or the connection drops before the handler answers",
			srv.WriteTimeout, requestTimeout)
	}
	if chartartifact.ProbeTimeout >= requestTimeout {
		t.Fatalf("the probe timeout %s must expire well inside the request budget %s",
			chartartifact.ProbeTimeout, requestTimeout)
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

func TestHandleInvalidRequestOutcome(t *testing.T) {
	rec := do(t, fakeResolver{result: resolver.Result{Outcome: resolver.OutcomeInvalidRequest, Message: "detail"}}, validBody)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	assertCode(t, rec.Body.Bytes(), "code", "INVALID_REQUEST")
}

func TestHandleUnknownRepositoryKind(t *testing.T) {
	rec := do(t, fakeResolver{}, `{"repositoryKind":"SomethingElse","repositoryName":"github","chart":"podinfo","version":"6.7.1"}`)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	assertCode(t, rec.Body.Bytes(), "code", "UNSUPPORTED_REPOSITORY_KIND")
	assertCode(t, rec.Body.Bytes(), "error", `unsupported repository kind "SomethingElse"`)
}

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

func TestHandleRequestTooLarge(t *testing.T) {
	oversized := `{"repositoryKind":"HelmClusterAddonRepository","repositoryName":"github","chart":"` +
		strings.Repeat("a", maxRequestBodyBytes) + `","version":"6.7.1"}`

	rec := do(t, fakeResolver{}, oversized)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", rec.Code)
	}
	assertCode(t, rec.Body.Bytes(), "code", "REQUEST_TOO_LARGE")
}

func TestHandleMissingTokenRejectsBeforeReadingAnOversizedBody(t *testing.T) {
	oversized := `{"repositoryKind":"HelmClusterAddonRepository","repositoryName":"github","chart":"` +
		strings.Repeat("a", maxRequestBodyBytes) + `","version":"6.7.1"}`

	srv := New("", fakeResolver{}, authorized, NewOptions{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chart-values", strings.NewReader(oversized))
	srv.handleChartValues(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	assertCode(t, rec.Body.Bytes(), "code", "UNAUTHENTICATED")
}

func TestNewHTTPServerSetsEveryTimeout(t *testing.T) {
	srv := newHTTPServer("", http.NewServeMux())

	if srv.ReadHeaderTimeout <= 0 {
		t.Fatal("ReadHeaderTimeout is not set")
	}
	if srv.ReadTimeout <= 0 {
		t.Fatal("ReadTimeout is not set")
	}
	if srv.WriteTimeout <= 0 {
		t.Fatal("WriteTimeout is not set")
	}
}

func TestHandleValidatesRepositoryName(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{
			name: "not a valid object name",
			body: `{"repositoryKind":"HelmClusterAddonRepository","repositoryName":"Not Valid!","chart":"podinfo","version":"6.7.1"}`,
		},
		{
			name: "shorter than the application repository CRD allows",
			body: `{"repositoryKind":"HelmApplicationRepository","namespace":"team-a","repositoryName":"ab","chart":"podinfo","version":"6.7.1"}`,
		},
		{
			name: "longer than the application repository CRD allows",
			body: `{"repositoryKind":"HelmApplicationRepository","namespace":"team-a","repositoryName":"` +
				strings.Repeat("a", 64) + `","chart":"podinfo","version":"6.7.1"}`,
		},
		{
			name: "longer than the cluster application repository CRD allows",
			body: `{"repositoryKind":"HelmClusterApplicationRepository","namespace":"team-a","repositoryName":"` +
				strings.Repeat("a", 64) + `","chart":"podinfo","version":"6.7.1"}`,
		},
		{
			name: "longer than the addon repository CRD allows",
			body: `{"repositoryKind":"HelmClusterAddonRepository","repositoryName":"` +
				strings.Repeat("a", 64) + `","chart":"podinfo","version":"6.7.1"}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := do(t, fakeResolver{}, tc.body)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %s)", rec.Code, rec.Body.String())
			}
			assertCode(t, rec.Body.Bytes(), "code", "INVALID_REQUEST")
		})
	}
}

func TestHandleAcceptsAShortAddonRepositoryName(t *testing.T) {
	rec := do(t, fakeResolver{result: resolver.Result{Outcome: resolver.OutcomeReady}},
		`{"repositoryKind":"HelmClusterAddonRepository","repositoryName":"ab","chart":"podinfo","version":"6.7.1"}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
}

func TestHandleValidatesChartAndVersion(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{
			name: "a whitespace-only version never matches a catalog entry and would poll pending forever",
			body: `{"repositoryKind":"HelmClusterAddonRepository","repositoryName":"github","chart":"podinfo","version":" "}`,
		},
		{
			name: "a whitespace-only chart",
			body: `{"repositoryKind":"HelmClusterAddonRepository","repositoryName":"github","chart":"   ","version":"6.7.1"}`,
		},
		{
			name: "a chart longer than the bound",
			body: `{"repositoryKind":"HelmClusterAddonRepository","repositoryName":"github","chart":"` +
				strings.Repeat("a", maxChartLen+1) + `","version":"6.7.1"}`,
		},
		{
			name: "a version longer than the OCI tag bound",
			body: `{"repositoryKind":"HelmClusterAddonRepository","repositoryName":"github","chart":"podinfo","version":"` +
				strings.Repeat("1", maxVersionLen+1) + `"}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := do(t, fakeResolver{}, tc.body)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %s)", rec.Code, rec.Body.String())
			}
			assertCode(t, rec.Body.Bytes(), "code", "INVALID_REQUEST")
		})
	}
}

func TestHandleAcceptsAChartNameWithASpace(t *testing.T) {
	rec := do(t, fakeResolver{result: resolver.Result{Outcome: resolver.OutcomeReady}},
		`{"repositoryKind":"HelmClusterAddonRepository","repositoryName":"github","chart":"my chart","version":"6.7.1"}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
}
