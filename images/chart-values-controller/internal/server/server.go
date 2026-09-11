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

// retryAfterSeconds is advertised to clients while an artifact is still being
// prepared, so polling clients back off consistently.
const retryAfterSeconds = 3

type chartValuesResolver interface {
	Resolve(ctx context.Context, req resolver.Request) (resolver.Result, error)
}

type tokenReviewer interface {
	Review(ctx context.Context, token string, access auth.Access) (auth.Result, error)
}

// Server exposes the chart-values HTTP API as a controller-runtime Runnable.
// It does not require leader election so it can serve from any replica.
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
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chart-values", s.handleChartValues)

	srv := &http.Server{
		Addr:              s.addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		<-ctx.Done()
		// The manager context is already cancelled here, so shutdown must run on a
		// fresh context with its own deadline.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx) //nolint:contextcheck // parent ctx is done, a fresh one is required for graceful shutdown
	}()

	serve := srv.ListenAndServe
	if s.tlsCertFile != "" && s.tlsKeyFile != "" {
		serve = func() error {
			return srv.ListenAndServeTLS(s.tlsCertFile, s.tlsKeyFile)
		}
	}

	if err := serve(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}

	return nil
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

	var req chartValuesRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "request body is not valid JSON")
		return
	}

	if req.RepositoryKind == "" || req.RepositoryName == "" || req.Chart == "" || req.Version == "" {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST",
			"repositoryKind, repositoryName, chart and version are required")
		return
	}

	// Answering exposes the values that feed into the resource of the repository's
	// family, so the caller must be allowed to create one — in the namespace it
	// would be created in, when that resource is namespaced.
	access, displayKind, namespaced, ok := accessFor(req.RepositoryKind, req.Namespace)
	if !ok {
		writeError(w, http.StatusBadRequest, "UNSUPPORTED_REPOSITORY_KIND",
			fmt.Sprintf("unsupported repository kind %q", req.RepositoryKind))
		return
	}
	if namespaced {
		// A name no namespace could carry would otherwise travel as far as the
		// access review and come back as a 403, which tells the caller nothing
		// about the field they got wrong.
		if errs := validation.IsDNS1123Label(req.Namespace); len(errs) > 0 {
			writeError(w, http.StatusBadRequest, "INVALID_REQUEST",
				"namespace is required for this repository kind and must be a valid namespace name")

			return
		}
	}
	if !s.authorize(w, r, access, displayKind) {
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
		logger.Error(err, "failed to resolve chart values")
		writeError(w, http.StatusInternalServerError, "INTERNAL", "internal server error")
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

// accessFor maps a repository kind to the permission that answering for it
// requires, plus the Kubernetes kind that permission is expressed in (for use in a
// message to the caller) and whether that permission is a namespaced question. Both
// application kinds are namespaced: even the cluster-wide repository's values reach
// a HelmApplication that lives in a namespace. Values of a chart from an application
// repository — namespaced or cluster-wide — feed into a HelmApplication in the
// request's namespace, so that is what the caller must be allowed to create; values
// from an addon repository feed into the cluster-scoped HelmClusterAddon.
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

// authorize reviews the request's bearer token against access and reports whether
// it may proceed. On any negative outcome it writes the response itself and returns
// false. displayKind names the Kubernetes kind access.Resource stands for, for the
// FORBIDDEN message.
func (s *Server) authorize(w http.ResponseWriter, r *http.Request, access auth.Access, displayKind string) bool {
	logger := log.FromContext(r.Context())

	token, ok := bearerToken(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "UNAUTHENTICATED", "missing or malformed Authorization header")
		return false
	}

	result, err := s.reviewer.Review(r.Context(), token, access)
	if err != nil {
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
