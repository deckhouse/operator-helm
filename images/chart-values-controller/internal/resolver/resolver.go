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

package resolver

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	neturl "net/url"
	"runtime/debug"
	"strings"
	"time"

	"github.com/fluxcd/pkg/apis/meta"
	sourcev1 "github.com/fluxcd/source-controller/api/v1"
	"golang.org/x/sync/singleflight"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/deckhouse/chart-values-controller/internal/artifact"
	"github.com/deckhouse/chart-values-controller/internal/cache"
	"github.com/deckhouse/chart-values-controller/internal/chartartifact"
	"github.com/deckhouse/chart-values-controller/internal/labels"
	"github.com/deckhouse/chart-values-controller/internal/naming"
	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
)

// RepositoryKind identifies the kind of repository a chart lives in. New
// repository kinds are added as new constants plus a registry entry in families.
type RepositoryKind string

const (
	// RepositoryKindHelmClusterAddon is a chart referenced by a HelmClusterAddonRepository.
	// Kind values are stored and compared in lower case, so the request casing does not matter.
	RepositoryKindHelmClusterAddon RepositoryKind = "helmclusteraddonrepository"

	// RepositoryKindHelmApplication is a chart referenced by a HelmApplicationRepository,
	// the namespaced repository of the application family.
	RepositoryKindHelmApplication RepositoryKind = "helmapplicationrepository"

	// RepositoryKindHelmClusterApplication is a chart referenced by a
	// HelmClusterApplicationRepository, the cluster-wide repository of that family.
	RepositoryKindHelmClusterApplication RepositoryKind = "helmclusterapplicationrepository"
)

// Outcome enumerates the possible results of resolving a chart-values request.
type Outcome string

const (
	OutcomeReady                     Outcome = "ready"
	OutcomePending                   Outcome = "pending"
	OutcomeRepositoryNotFound        Outcome = "repository_not_found"
	OutcomeUnsupportedRepositoryKind Outcome = "unsupported_repository_kind"
	OutcomeFetchFailed               Outcome = "fetch_failed"
	OutcomeValuesNotFound            Outcome = "values_not_found"

	// OutcomeInvalidRequest means the request itself does not make sense for the
	// kind it names — a namespaced kind without a namespace.
	OutcomeInvalidRequest Outcome = "invalid_request"
)

// Request identifies a chart by repository kind, namespace, repository name, chart name
// and version. Namespace identifies the repository only for a namespaced kind.
type Request struct {
	Kind           RepositoryKind
	Namespace      string
	RepositoryName string
	Chart          string
	Version        string
}

// Result is the outcome of a Resolve call. Values is populated only when
// Outcome is OutcomeReady; Message carries human-readable detail for failures.
type Result struct {
	Outcome Outcome
	Values  []byte
	Message string
}

type Resolver struct {
	client           client.Client
	cache            *cache.Cache
	httpClient       *http.Client
	namespace        string
	ttl              time.Duration
	sourceInterval   metav1.Duration
	maxArtifactBytes int64
	prober           chartartifact.Prober

	// downloads collapses concurrent misses on one artifact revision of one name into a single download.
	downloads singleflight.Group
}

func New(c client.Client, valuesCache *cache.Cache, httpClient *http.Client, namespace string, ttl, sourceInterval time.Duration, maxArtifactBytes int64) *Resolver {
	return &Resolver{
		client:           c,
		cache:            valuesCache,
		httpClient:       httpClient,
		namespace:        namespace,
		ttl:              ttl,
		sourceInterval:   metav1.Duration{Duration: sourceInterval},
		maxArtifactBytes: maxArtifactBytes,
		prober:           chartartifact.Default,
	}
}

// Resolve dispatches on the repository kind and, when the artifact is ready, returns the
// chart's values.yaml. A returned error is an internal failure; every expected state is an Outcome.
func (r *Resolver) Resolve(ctx context.Context, req Request) (Result, error) {
	requested := req.Kind
	req.Kind = RepositoryKind(strings.ToLower(string(req.Kind)))

	family, ok := familyFor(req.Kind)
	if !ok {
		return Result{Outcome: OutcomeUnsupportedRepositoryKind, Message: fmt.Sprintf("unsupported repository kind %q", requested)}, nil
	}

	if err := family.requireNamespace(req.Namespace); err != nil {
		return Result{Outcome: OutcomeInvalidRequest, Message: err.Error()}, nil
	}

	if !family.Namespaced {
		// For a cluster-scoped family the namespace is the caller's authorization context only.
		req.Namespace = ""
	}

	return r.resolveChart(ctx, family, req)
}

func (r *Resolver) chartVersion(ctx context.Context, family repositoryFamily, req Request) (*helmv1alpha1.ChartVersion, *Result, error) {
	versions, err := family.ChartVersions(ctx, r.client, req.Namespace, req.RepositoryName, req.Chart)
	if err != nil {
		if apierrors.IsNotFound(err) {
			// The chart object is created by operator-helm-controller when it syncs the repository.
			return nil, &Result{Outcome: OutcomePending}, nil
		}

		return nil, nil, fmt.Errorf("getting chart: %w", err)
	}

	for i := range versions {
		version := &versions[i]
		if version.Version != req.Version {
			continue
		}

		if version.UnavailableReason == helmv1alpha1.UnavailableReasonInvalidChartReference {
			return nil, &Result{
				Outcome: OutcomeValuesNotFound,
				Message: fmt.Sprintf("chart version %s is not readable (%s)", req.Version, versionDetail(version)),
			}, nil
		}

		return version, nil, nil
	}

	return nil, &Result{Outcome: OutcomePending}, nil
}

func ociMediaType(req Request, version *helmv1alpha1.ChartVersion) (string, *Result) {
	if version.MediaType != "" {
		return version.MediaType, nil
	}

	if version.UnavailableReason == helmv1alpha1.UnavailableReasonResolvePending || version.UnavailableReason == "" {
		// An empty reason with an empty media type is the pre-upgrade shape, re-resolved like pending.
		return "", &Result{Outcome: OutcomePending}
	}

	return "", &Result{
		Outcome: OutcomeValuesNotFound,
		Message: fmt.Sprintf("chart version %s is not readable (%s)", req.Version, versionDetail(version)),
	}
}

func versionDetail(version *helmv1alpha1.ChartVersion) string {
	detail := version.UnavailableReason
	if version.UnavailableMessage != "" {
		detail += ": " + version.UnavailableMessage
	}

	return detail
}

func (r *Resolver) resolveChart(ctx context.Context, family repositoryFamily, req Request) (Result, error) {
	repo, err := family.GetRepository(ctx, r.client, req.Namespace, req.RepositoryName)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return Result{Outcome: OutcomeRepositoryNotFound, Message: fmt.Sprintf("repository %q not found", req.RepositoryName)}, nil
		}
		return Result{}, fmt.Errorf("getting repository: %w", err)
	}

	name := naming.AuxResourceName(string(req.Kind), req.Namespace, req.RepositoryName, req.Chart, req.Version, repo.Identity)

	expiresAt := time.Now().UTC().Add(r.ttl).Format(time.RFC3339)

	version, done, err := r.chartVersion(ctx, family, req)
	if err != nil {
		return Result{}, err
	}
	if done != nil {
		return *done, nil
	}

	if obj := sourceObjectFor(repo.URL, version); obj != nil {
		values, ok, err := r.cachedValues(ctx, name, obj)
		if err != nil {
			return Result{}, err
		}
		if ok {
			return Result{Outcome: OutcomeReady, Values: values}, nil
		}
	}

	var conditions []metav1.Condition
	var art *meta.Artifact

	switch {
	case version.OCIRef != "":
		ociRepo, done, err := r.ensureHybridOCIRepository(ctx, family, repo, req, name, expiresAt, version)
		if err != nil {
			return Result{}, err
		}
		if done != nil {
			return *done, nil
		}
		conditions, art = ociRepo.Status.Conditions, ociRepo.Status.Artifact
	case isOCI(repo.URL):
		mediaType, done := ociMediaType(req, version)
		if done != nil {
			return *done, nil
		}

		ociRepo, err := r.ensureOCIRepository(ctx, family, repo, req, name, expiresAt, repo.URL, req.Version, mediaType, true, true)
		if err != nil {
			return Result{}, err
		}
		conditions, art = ociRepo.Status.Conditions, ociRepo.Status.Artifact
	case isHelm(repo.URL):
		chart, pending, err := r.ensureHelmChart(ctx, family, req, name, expiresAt)
		if err != nil {
			return Result{}, err
		}
		if pending {
			return Result{Outcome: OutcomePending}, nil
		}
		conditions, art = chart.Status.Conditions, chart.Status.Artifact
	default:
		return Result{Outcome: OutcomeFetchFailed, Message: fmt.Sprintf("unsupported repository URL scheme: %q", repo.URL)}, nil
	}

	// Flux keeps the artifact across a failed reconcile, so the revision it names is still served.
	if art != nil {
		if values, ok := r.cache.Get(name, art.Digest); ok {
			return Result{Outcome: OutcomeReady, Values: values}, nil
		}

		return r.readValues(ctx, name, art)
	}

	outcome, message := classify(conditions)

	return Result{Outcome: outcome, Message: message}, nil
}

func sourceObjectFor(repoURL string, version *helmv1alpha1.ChartVersion) client.Object {
	switch {
	case version.OCIRef != "" || isOCI(repoURL):
		return &sourcev1.OCIRepository{}
	case isHelm(repoURL):
		return &sourcev1.HelmChart{}
	default:
		return nil
	}
}

func sourceArtifact(obj client.Object) *meta.Artifact {
	switch source := obj.(type) {
	case *sourcev1.HelmChart:
		return source.Status.Artifact
	case *sourcev1.OCIRepository:
		return source.Status.Artifact
	default:
		return nil
	}
}

func (r *Resolver) cachedValues(ctx context.Context, name string, obj client.Object) ([]byte, bool, error) {
	if err := r.client.Get(ctx, client.ObjectKey{Namespace: r.namespace, Name: name}, obj); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, false, nil
		}

		return nil, false, fmt.Errorf("getting auxiliary source object: %w", err)
	}

	expiresAt, err := time.Parse(time.RFC3339, obj.GetAnnotations()[labels.ExpiresAtAnnotation])
	// Past half the ttl the slow path runs to renew the expiry before cleanup takes the object.
	if err != nil || time.Until(expiresAt) <= r.ttl/2 {
		return nil, false, nil
	}

	art := sourceArtifact(obj)
	if art == nil {
		return nil, false, nil
	}

	values, ok := r.cache.Get(name, art.Digest)

	return values, ok, nil
}

// sharedDownloadTimeout bounds the shared call as a whole, since the call is detached from the
// request that started it: other requests may be waiting on its result. The HTTP client's own
// timeout already bounds the download itself.
const sharedDownloadTimeout = 90 * time.Second

// shortcut: a superseded revision stays cached until its object expires, and a download that outlasts the expiry leaves an orphan entry until restart; add a timestamp to entries if the budget ever fills with them.
func (r *Resolver) readValues(ctx context.Context, name string, art *meta.Artifact) (Result, error) {
	ch := r.downloads.DoChan(name+"@"+art.Digest, func() (any, error) {
		return r.download(ctx, name, art)
	})

	var values []byte
	var err error
	select {
	case res := <-ch:
		values, _ = res.Val.([]byte)
		err = res.Err
	case <-ctx.Done():
		return Result{Outcome: OutcomePending}, nil
	}
	if err != nil {
		if errors.Is(err, artifact.ErrValuesNotFound) {
			return Result{Outcome: OutcomeValuesNotFound, Message: "chart has no values.yaml"}, nil
		}

		if isTransient(err) {
			return Result{Outcome: OutcomePending}, nil
		}

		return Result{Outcome: OutcomeFetchFailed, Message: err.Error()}, nil
	}

	return Result{Outcome: OutcomeReady, Values: values}, nil
}

func (r *Resolver) download(ctx context.Context, name string, art *meta.Artifact) (values []byte, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			log.FromContext(ctx).Error(nil, "panic while downloading values.yaml", "name", name, "panic", recovered, "stack", string(debug.Stack()))
			values, err = nil, fmt.Errorf("downloading %s: %v", name, recovered)
		}
	}()

	downloadCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sharedDownloadTimeout)
	defer cancel()

	values, err = artifact.FetchValues(downloadCtx, r.httpClient, art.URL, art.Digest, r.maxArtifactBytes)
	if err != nil {
		return nil, err
	}

	if err := r.cache.Put(name, art.Digest, values); err != nil {
		log.FromContext(ctx).Info("not caching values.yaml, cache budget exhausted", "name", name)
	}

	return values, nil
}

func isTransient(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

func (r *Resolver) ensureHelmChart(ctx context.Context, family repositoryFamily, req Request, name, expiresAt string) (*sourcev1.HelmChart, bool, error) {
	helmRepoName, err := r.findHelmRepositoryName(ctx, family, req)
	if err != nil {
		return nil, false, err
	}
	if helmRepoName == "" {
		// The backing HelmRepository is created by operator-helm-controller.
		return nil, true, nil
	}

	chart := &sourcev1.HelmChart{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: r.namespace,
		},
	}

	if _, err := controllerutil.CreateOrPatch(ctx, r.client, chart, func() error {
		applyManagedMeta(chart, expiresAt)
		chart.Spec.Chart = req.Chart
		chart.Spec.Version = req.Version
		chart.Spec.SourceRef = sourcev1.LocalHelmChartSourceReference{
			Kind: sourcev1.HelmRepositoryKind,
			Name: helmRepoName,
		}
		chart.Spec.Interval = r.sourceInterval

		return nil
	}); err != nil {
		return nil, false, fmt.Errorf("ensuring helm chart: %w", err)
	}

	return chart, false, nil
}

func (r *Resolver) ensureOCIRepository(
	ctx context.Context,
	family repositoryFamily,
	repo *repositorySpec,
	req Request,
	name, expiresAt, url, tag, mediaType string,
	credentials, tls bool,
) (*sourcev1.OCIRepository, error) {
	authSecret, tlsSecret, err := r.findRepositorySecretNames(ctx, family, req)
	if err != nil {
		return nil, err
	}

	ociRepo := &sourcev1.OCIRepository{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: r.namespace,
		},
	}

	if _, err := controllerutil.CreateOrPatch(ctx, r.client, ociRepo, func() error {
		applyManagedMeta(ociRepo, expiresAt)
		ociRepo.Spec.URL = url
		ociRepo.Spec.Reference = &sourcev1.OCIRepositoryRef{Tag: tag}
		ociRepo.Spec.Interval = r.sourceInterval
		ociRepo.Spec.LayerSelector = &sourcev1.OCILayerSelector{
			MediaType: mediaType,
			Operation: "copy",
		}

		ociRepo.Spec.Insecure = false
		ociRepo.Spec.SecretRef = nil
		ociRepo.Spec.CertSecretRef = nil
		if tls {
			ociRepo.Spec.Insecure = repo.InsecureSkipVerify
			if repo.CACertificate != "" && tlsSecret != "" {
				ociRepo.Spec.CertSecretRef = &meta.LocalObjectReference{Name: tlsSecret}
			}
		}
		if credentials && repo.Auth != nil && authSecret != "" {
			ociRepo.Spec.SecretRef = &meta.LocalObjectReference{Name: authSecret}
		}

		return nil
	}); err != nil {
		return nil, fmt.Errorf("ensuring oci repository: %w", err)
	}

	return ociRepo, nil
}

func (r *Resolver) ensureHybridOCIRepository(
	ctx context.Context,
	family repositoryFamily,
	repo *repositorySpec,
	req Request,
	name, expiresAt string,
	version *helmv1alpha1.ChartVersion,
) (*sourcev1.OCIRepository, *Result, error) {
	url, tag, err := helmv1alpha1.SplitOCIRef(version.OCIRef, "")
	if err != nil {
		return nil, &Result{Outcome: OutcomeFetchFailed, Message: err.Error()}, nil
	}

	// The repository's transport settings describe the host it names, so another registry is public.
	sameHost := sameRegistryHost(repo.URL, url)

	mediaType := version.MediaType
	if mediaType == "" {
		var rt http.RoundTripper
		if sameHost {
			rt, err = chartartifact.Transport(repo.CACertificate, repo.InsecureSkipVerify)
			if err != nil {
				return nil, &Result{Outcome: OutcomeFetchFailed, Message: err.Error()}, nil
			}
		}

		mediaType, err = r.prober.ChartLayerMediaType(ctx, version.OCIRef, rt)
		if err != nil {
			if errors.Is(err, chartartifact.ErrNotAChart) || errors.Is(err, chartartifact.ErrTagNotFound) {
				return nil, &Result{Outcome: OutcomeValuesNotFound, Message: err.Error()}, nil
			}

			log.FromContext(ctx).Info("Probing the chart artifact failed", "ref", version.OCIRef, "error", err.Error())

			return nil, &Result{Outcome: OutcomePending}, nil
		}
	}

	ociRepo, err := r.ensureOCIRepository(ctx, family, repo, req, name, expiresAt, url, tag, mediaType, false, sameHost)
	if err != nil {
		return nil, nil, err
	}

	return ociRepo, nil, nil
}

func sameRegistryHost(repoURL, artifactURL string) bool {
	repoHost, err := neturl.Parse(repoURL)
	if err != nil || repoHost.Host == "" {
		return false
	}

	artifactHost, err := neturl.Parse(artifactURL)
	if err != nil || artifactHost.Host == "" {
		return false
	}

	return repoHost.Host == artifactHost.Host
}

func (r *Resolver) findHelmRepositoryName(ctx context.Context, family repositoryFamily, req Request) (string, error) {
	var list sourcev1.HelmRepositoryList
	if err := r.client.List(
		ctx, &list,
		client.InNamespace(r.namespace),
		client.MatchingLabels(family.InternalLabels(req.Namespace, req.RepositoryName)),
	); err != nil {
		return "", fmt.Errorf("listing helm repositories: %w", err)
	}

	if len(list.Items) == 0 {
		return "", nil
	}

	return list.Items[0].Name, nil
}

func (r *Resolver) findRepositorySecretNames(ctx context.Context, family repositoryFamily, req Request) (auth, tls string, err error) {
	var list corev1.SecretList
	if err := r.client.List(
		ctx, &list,
		client.InNamespace(r.namespace),
		client.MatchingLabels(family.InternalLabels(req.Namespace, req.RepositoryName)),
	); err != nil {
		return "", "", fmt.Errorf("listing repository secrets: %w", err)
	}

	for i := range list.Items {
		secret := &list.Items[i]
		// The auth secret carries username/password or dockerconfigjson, so either key marks it.
		_, hasBasicAuth := secret.Data["username"]
		_, hasDockerConfig := secret.Data[corev1.DockerConfigJsonKey]
		if hasBasicAuth || hasDockerConfig {
			auth = secret.Name
			continue
		}
		if _, ok := secret.Data["ca.crt"]; ok {
			tls = secret.Name
		}
	}

	return auth, tls, nil
}

func classify(conditions []metav1.Condition) (Outcome, string) {
	for _, conditionType := range []string{"FetchFailed", "StorageOperationFailed", "BuildFailed"} {
		if c := apimeta.FindStatusCondition(conditions, conditionType); c != nil && c.Status == metav1.ConditionTrue {
			return OutcomeFetchFailed, c.Message
		}
	}

	return OutcomePending, ""
}

func applyManagedMeta(obj client.Object, expiresAt string) {
	objLabels := obj.GetLabels()
	if objLabels == nil {
		objLabels = map[string]string{}
	}
	objLabels[labels.ManagedBy] = labels.ManagedByValue
	obj.SetLabels(objLabels)

	annotations := obj.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations[labels.ExpiresAtAnnotation] = expiresAt
	obj.SetAnnotations(annotations)
}

func isOCI(url string) bool {
	return strings.HasPrefix(url, "oci://")
}

func isHelm(url string) bool {
	return strings.HasPrefix(url, "http://") || strings.HasPrefix(url, "https://")
}
