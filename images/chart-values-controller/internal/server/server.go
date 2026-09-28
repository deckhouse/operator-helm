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
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/deckhouse/chart-values-controller/internal/auth"
	"github.com/deckhouse/chart-values-controller/internal/resolver"
	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
)

// retryAfterSeconds is advertised to clients while an artifact is still being prepared.
const retryAfterSeconds = 3

const (
	// maxRequestBodyBytes bounds the request body before it is decoded, generously over the five short string fields a legitimate request carries.
	maxRequestBodyBytes = 4 << 10

	// requestTimeout is the budget for one request end to end, carried on its context so every step inherits it. It is set above the slowest legitimate answer: a probe followed by the download of a whole chart artifact.
	requestTimeout = 90 * time.Second

	// readTimeout and writeTimeout bound how long a connection may take to send its body or receive its response.
	// WriteTimeout runs from the moment the headers are read, so it is requestTimeout plus room for the handler to write its answer.
	readTimeout         = 10 * time.Second
	writeResponseMargin = 30 * time.Second
	writeTimeout        = requestTimeout + writeResponseMargin

	// maxChartLen bounds the chart field, which carries no naming grammar of its own.
	maxChartLen = 253

	// maxRepositoryNameLen bounds the repositoryName field at what every repository CRD enforces on metadata.name, which is the length a label value holds.
	maxRepositoryNameLen = 63

	// maxVersionLen bounds the version field at the OCI Distribution Spec's tag length limit, since a version may travel on as an OCI tag.
	maxVersionLen = 128
)

type chartValuesResolver interface {
	Resolve(ctx context.Context, req resolver.Request) (resolver.Result, error)
}

type tokenReviewer interface {
	Review(ctx context.Context, token string, access auth.Access) (auth.Result, error)
}

// Server exposes the chart-values HTTP API as a controller-runtime Runnable that
// needs no leader election.
type Server struct {
	addr        string
	resolver    chartValuesResolver
	reviewer    tokenReviewer
	tlsCertFile string
	tlsKeyFile  string
}

// NewOptions carries optional server configuration. When both TLSCertFile and
// TLSKeyFile are set the API is served over TLS.
type NewOptions struct {
	TLSCertFile string
	TLSKeyFile  string
}

func New(addr string, res chartValuesResolver, reviewer tokenReviewer, opts NewOptions) *Server {
	return &Server{
		addr:        addr,
		resolver:    res,
		reviewer:    reviewer,
		tlsCertFile: opts.TLSCertFile,
		tlsKeyFile:  opts.TLSKeyFile,
	}
}

var _ tokenReviewer = (*auth.Reviewer)(nil)

// NeedLeaderElection reports that the HTTP server runs on every replica.
func (s *Server) NeedLeaderElection() bool {
	return false
}

func (s *Server) Start(ctx context.Context) error {
	srv := newHTTPServer(s.addr, s.handler())

	go func() {
		<-ctx.Done()
		// The manager context is already cancelled here, so shutdown runs on a fresh one.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx) //nolint:contextcheck // parent ctx is done, a fresh one is required for graceful shutdown
	}()

	serve := srv.ListenAndServe
	if s.tlsCertFile != "" && s.tlsKeyFile != "" {
		tlsConfig, err := newTLSConfig(ctx, s.tlsCertFile, s.tlsKeyFile)
		if err != nil {
			return err
		}
		srv.TLSConfig = tlsConfig

		// Both arguments are empty on purpose: passing the paths would read them once and pin that read, instead of serving TLSConfig's GetCertificate.
		serve = func() error {
			return srv.ListenAndServeTLS("", "")
		}
	}

	if err := serve(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}

	return nil
}

// handler builds the API's routes under the request budget, which a middleware over the mux puts on every request the API serves.
func (s *Server) handler() http.Handler {
	return withRequestDeadline(requestTimeout, s.routes())
}

func (s *Server) routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chart-values", s.handleChartValues)

	return mux
}

// withRequestDeadline gives every request one deadline that all of its steps inherit through the context, and only ever narrows it.
func withRequestDeadline(timeout time.Duration, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), timeout)
		defer cancel()

		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// newHTTPServer builds the http.Server the API is served through, with a timeout on each of the request line, the body and the response.
func newHTTPServer(addr string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
	}
}

type chartValuesRequest struct {
	RepositoryKind string `json:"repositoryKind"`
	Namespace      string `json:"namespace"`
	RepositoryName string `json:"repositoryName"`
	Chart          string `json:"chart"`
	Version        string `json:"version"`
}

func (s *Server) handleChartValues(w http.ResponseWriter, r *http.Request) {
	logger := log.FromContext(r.Context())

	// The bearer token is read from the header alone, so an unauthenticated caller is rejected before the body is read.
	token, ok := bearerToken(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "UNAUTHENTICATED", "missing or malformed Authorization header")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)

	var req chartValuesRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "REQUEST_TOO_LARGE", "request body exceeds the size limit")
			return
		}
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "request body is not valid JSON")
		return
	}

	if req.RepositoryKind == "" || req.RepositoryName == "" || req.Chart == "" || req.Version == "" {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST",
			"repositoryKind, repositoryName, chart and version are required")
		return
	}

	// Answering exposes the values that feed into the resource of the repository's family, so the caller must be allowed to create one.
	access, displayKind, namespaced, ok := accessFor(req.RepositoryKind, req.Namespace)
	if !ok {
		writeError(w, http.StatusBadRequest, "UNSUPPORTED_REPOSITORY_KIND",
			fmt.Sprintf("unsupported repository kind %q", req.RepositoryKind))
		return
	}
	if namespaced {
		// A name no namespace could carry would otherwise come back from the access review as a 403.
		if errs := validation.IsDNS1123Label(req.Namespace); len(errs) > 0 {
			writeError(w, http.StatusBadRequest, "INVALID_REQUEST",
				"namespace is required for this repository kind and must be a valid namespace name")

			return
		}
	}
	if err := validateChartValuesFields(req); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", err.Error())
		return
	}

	// The access review needs the resource derived above, so it cannot run any earlier.
	if !s.authorize(w, r, token, access, displayKind) {
		return
	}

	result, err := s.resolver.Resolve(r.Context(), resolver.Request{
		Kind:           resolver.RepositoryKind(strings.ToLower(req.RepositoryKind)),
		Namespace:      req.Namespace,
		RepositoryName: req.RepositoryName,
		Chart:          req.Chart,
		Version:        req.Version,
	})
	if err != nil {
		if budgetExhausted(w, r) {
			return
		}

		logger.Error(err, "failed to resolve chart values")
		writeError(w, http.StatusInternalServerError, "INTERNAL", "internal server error")
		return
	}

	// A step that ran out of budget reports it as its own kind of failure, so the budget is consulted before the outcome is mapped.
	if result.Outcome != resolver.OutcomeReady && budgetExhausted(w, r) {
		return
	}

	switch result.Outcome {
	case resolver.OutcomeReady:
		writeJSON(w, http.StatusOK, map[string]any{
			"data": map[string]string{"values.yaml": string(result.Values)},
		})
	case resolver.OutcomePending:
		w.Header().Set("Retry-After", strconv.Itoa(retryAfterSeconds))
		writeJSON(w, http.StatusAccepted, map[string]any{
			"status":  "pending",
			"code":    "CHART_PENDING",
			"message": "chart artifact is being prepared, retry later",
		})
	case resolver.OutcomeRepositoryNotFound:
		writeError(w, http.StatusNotFound, "REPOSITORY_NOT_FOUND", result.Message)
	case resolver.OutcomeUnsupportedRepositoryKind:
		writeError(w, http.StatusBadRequest, "UNSUPPORTED_REPOSITORY_KIND", result.Message)
	case resolver.OutcomeValuesNotFound:
		writeError(w, http.StatusUnprocessableEntity, "VALUES_NOT_FOUND", result.Message)
	case resolver.OutcomeFetchFailed:
		writeError(w, http.StatusBadGateway, "CHART_FETCH_FAILED", result.Message)
	case resolver.OutcomeInvalidRequest:
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", result.Message)
	default:
		logger.Info("unexpected resolve outcome", "outcome", result.Outcome)
		writeError(w, http.StatusInternalServerError, "INTERNAL", "internal server error")
	}
}

// accessFor maps a repository kind to the permission answering for it requires, the Kubernetes kind that permission names, and whether it is a namespaced question.
func accessFor(kind, namespace string) (access auth.Access, displayKind string, namespaced, ok bool) {
	switch strings.ToLower(kind) {
	case string(resolver.RepositoryKindHelmClusterAddon):
		return auth.Access{
			Group:    helmv1alpha1.GroupName,
			Resource: helmv1alpha1.HelmClusterAddonResource,
			Verb:     "create",
		}, helmv1alpha1.HelmClusterAddonKind, false, true
	case string(resolver.RepositoryKindHelmApplication), string(resolver.RepositoryKindHelmClusterApplication):
		return auth.Access{
			Group:     helmv1alpha1.GroupName,
			Resource:  helmv1alpha1.HelmApplicationResource,
			Verb:      "create",
			Namespace: namespace,
		}, helmv1alpha1.HelmApplicationKind, true, true
	default:
		return auth.Access{}, "", false, false
	}
}

// repositoryNameMinLen returns the minimum length kind's own repository CRD enforces on metadata.name; zero means it imposes none, as HelmClusterAddonRepository shipped without one.
func repositoryNameMinLen(kind string) int {
	switch strings.ToLower(kind) {
	case string(resolver.RepositoryKindHelmApplication), string(resolver.RepositoryKindHelmClusterApplication):
		return 3
	default:
		return 0
	}
}

func validateChartValuesFields(req chartValuesRequest) error {
	if errs := validation.IsDNS1123Subdomain(req.RepositoryName); len(errs) > 0 {
		return fmt.Errorf("repositoryName must be a valid object name: %s", strings.Join(errs, "; "))
	}
	if minLen := repositoryNameMinLen(req.RepositoryKind); minLen > 0 && len(req.RepositoryName) < minLen {
		return fmt.Errorf("repositoryName must be at least %d characters long", minLen)
	}
	if len(req.RepositoryName) > maxRepositoryNameLen {
		return fmt.Errorf("repositoryName must be at most %d characters long", maxRepositoryNameLen)
	}

	// chart and version carry no naming grammar of their own: an index entry may contain a space and an OCI tag is not semver.
	if strings.TrimSpace(req.Chart) == "" {
		return errors.New("chart must not be blank")
	}
	if len(req.Chart) > maxChartLen {
		return fmt.Errorf("chart must be at most %d characters long", maxChartLen)
	}

	if strings.TrimSpace(req.Version) == "" {
		return errors.New("version must not be blank")
	}
	if len(req.Version) > maxVersionLen {
		return fmt.Errorf("version must be at most %d characters long", maxVersionLen)
	}

	return nil
}

// authorize reviews token against access and reports whether the request may proceed, writing the response itself on every negative outcome.
func (s *Server) authorize(w http.ResponseWriter, r *http.Request, token string, access auth.Access, displayKind string) bool {
	logger := log.FromContext(r.Context())

	result, err := s.reviewer.Review(r.Context(), token, access)
	if err != nil {
		if budgetExhausted(w, r) {
			return false
		}

		logger.Error(err, "failed to review request token")
		writeError(w, http.StatusInternalServerError, "INTERNAL", "internal server error")
		return false
	}
	if !result.Authenticated {
		writeError(w, http.StatusUnauthorized, "UNAUTHENTICATED", "token is not authenticated")
		return false
	}
	if !result.Authorized {
		writeError(w, http.StatusForbidden, "FORBIDDEN", fmt.Sprintf("not allowed to create %s", displayKind))
		return false
	}

	return true
}

// budgetExhausted reports whether the request's own budget ended the step that just failed, answering the client with a 504 when it did.
func budgetExhausted(w http.ResponseWriter, r *http.Request) bool {
	if !errors.Is(r.Context().Err(), context.DeadlineExceeded) {
		return false
	}

	writeError(w, http.StatusGatewayTimeout, "REQUEST_TIMEOUT",
		"request exceeded its time budget")

	return true
}

func bearerToken(r *http.Request) (string, bool) {
	const prefix = "bearer "

	header := r.Header.Get("Authorization")
	if len(header) < len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return "", false
	}

	token := strings.TrimSpace(header[len(prefix):])
	if token == "" {
		return "", false
	}

	return token, true
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{
		"error": message,
		"code":  code,
	})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
