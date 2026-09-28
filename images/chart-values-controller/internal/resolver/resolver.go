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
	"strings"
	"time"

	"github.com/fluxcd/pkg/apis/meta"
	sourcev1 "github.com/fluxcd/source-controller/api/v1"
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

	// downloads collapses concurrent cache misses on one key into a single download and extraction, keyed exactly like the cache.
	downloads inflight
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
	// Kind is case-insensitive, and the lower-cased form is what dispatches and feeds the resource name hash.
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
		// For a cluster-scoped family the namespace is the caller's authorization context only, and must reach neither the resource name nor the lookups.
		req.Namespace = ""
	}

	return r.resolveChart(ctx, family, req)
}

// chartVersion finds the catalog entry for the requested version; a missing chart object or a missing version is pending. A non-nil Result means the caller must stop and return it.
func (r *Resolver) chartVersion(ctx context.Context, family repositoryFamily, req Request) (*helmv1alpha1.ChartVersion, *Result, error) {
	versions, err := family.ChartVersions(ctx, r.client, req.Namespace, req.RepositoryName, req.Chart)
	if err != nil {
		if apierrors.IsNotFound(err) {
			// The chart object is created by operator-helm-controller when it synchronizes the repository.
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
			// Left through, this version would fall to the archive path and fail on the very same url as an opaque fetch error.
			return nil, &Result{
				Outcome: OutcomeValuesNotFound,
				Message: fmt.Sprintf("chart version %s is not readable (%s)", req.Version, versionDetail(version)),
			}, nil
		}

		return version, nil, nil
	}

	return nil, &Result{Outcome: OutcomePending}, nil
}

// ociMediaType reports the layer media type the catalog recorded for a version of an oci:// repository. A non-nil Result means the caller must stop and return it.
func ociMediaType(req Request, version *helmv1alpha1.ChartVersion) (string, *Result) {
	if version.MediaType != "" {
		return version.MediaType, nil
	}

	if version.UnavailableReason == helmv1alpha1.UnavailableReasonResolvePending || version.UnavailableReason == "" {
		// An empty reason alongside an empty media type is the pre-upgrade shape of a version entry, which the operator re-resolves like ResolvePending.
		return "", &Result{Outcome: OutcomePending}
	}

	// Every other reason is a durable verdict that will not change without a change in the repository.
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

// resolveChart ensures the auxiliary source resource for one chart exists, inspects its status and returns the chart's values.yaml once the artifact is ready.
func (r *Resolver) resolveChart(ctx context.Context, family repositoryFamily, req Request) (Result, error) {
	// Read first because the resource name, and with it the cache entry, carries the identity of the repository object the values came from.
	repo, err := family.GetRepository(ctx, r.client, req.Namespace, req.RepositoryName)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return Result{Outcome: OutcomeRepositoryNotFound, Message: fmt.Sprintf("repository %q not found", req.RepositoryName)}, nil
		}
		return Result{}, fmt.Errorf("getting repository: %w", err)
	}

	name := naming.AuxResourceName(string(req.Kind), req.Namespace, req.RepositoryName, req.Chart, req.Version, repo.Identity)

	// Freshness is the auxiliary-resource controller's statement — it replaces or removes an entry whose revision moved — so a hit is served without reading the source object.
	if values, ok := r.cache.Get(name); ok {
		return Result{Outcome: OutcomeReady, Values: values}, nil
	}

	expiresAt := time.Now().UTC().Add(r.ttl).Format(time.RFC3339)

	version, done, err := r.chartVersion(ctx, family, req)
	if err != nil {
		return Result{}, err
	}
	if done != nil {
		return *done, nil
	}

	var conditions []metav1.Condition
	var art *meta.Artifact
	// Which source object to ask is decided in the switch below and known nowhere after it.
	var current currentDigestFunc

	switch {
	case version.OCIRef != "":
		// The index publishes this version in a registry, so a HelmChart would send the source controller at the index url over HTTP and fail on the oci:// scheme.
		ociRepo, done, err := r.ensureHybridOCIRepository(ctx, family, repo, req, name, expiresAt, version)
		if err != nil {
			return Result{}, err
		}
		if done != nil {
			return *done, nil
		}
		conditions, art, current = ociRepo.Status.Conditions, ociRepo.Status.Artifact, r.ociRepositoryDigest(name)
	case isOCI(repo.URL):
		mediaType, done := ociMediaType(req, version)
		if done != nil {
			return *done, nil
		}

		ociRepo, err := r.ensureOCIRepository(ctx, family, repo, req, name, expiresAt, repo.URL, req.Version, mediaType, true, true)
		if err != nil {
			return Result{}, err
		}
		conditions, art, current = ociRepo.Status.Conditions, ociRepo.Status.Artifact, r.ociRepositoryDigest(name)
	case isHelm(repo.URL):
		chart, pending, err := r.ensureHelmChart(ctx, family, req, name, expiresAt)
		if err != nil {
			return Result{}, err
		}
		if pending {
			return Result{Outcome: OutcomePending}, nil
		}
		conditions, art, current = chart.Status.Conditions, chart.Status.Artifact, r.helmChartDigest(name)
	default:
		return Result{Outcome: OutcomeFetchFailed, Message: fmt.Sprintf("unsupported repository URL scheme: %q", repo.URL)}, nil
	}

	switch outcome, message := classify(conditions, art); outcome {
	case OutcomeFetchFailed:
		return Result{Outcome: OutcomeFetchFailed, Message: message}, nil
	case OutcomePending:
		return Result{Outcome: OutcomePending}, nil
	}

	return r.readValues(ctx, name, art, current)
}

// currentDigestFunc reports the digest of the artifact the auxiliary source object publishes now, read from the manager's informer cache.
type currentDigestFunc func(ctx context.Context) (string, error)

func (r *Resolver) helmChartDigest(name string) currentDigestFunc {
	return func(ctx context.Context) (string, error) {
		var chart sourcev1.HelmChart
		if err := r.client.Get(ctx, client.ObjectKey{Namespace: r.namespace, Name: name}, &chart); err != nil {
			return "", fmt.Errorf("getting helm chart: %w", err)
		}

		return artifactDigest(chart.Status.Artifact), nil
	}
}

func (r *Resolver) ociRepositoryDigest(name string) currentDigestFunc {
	return func(ctx context.Context) (string, error) {
		var ociRepo sourcev1.OCIRepository
		if err := r.client.Get(ctx, client.ObjectKey{Namespace: r.namespace, Name: name}, &ociRepo); err != nil {
			return "", fmt.Errorf("getting oci repository: %w", err)
		}

		return artifactDigest(ociRepo.Status.Artifact), nil
	}
}

// artifactDigest reads the revision off an artifact that may not be published at all.
func artifactDigest(art *meta.Artifact) string {
	if art == nil {
		return ""
	}

	return art.Digest
}

// readValues downloads the artifact and returns the chart's values.yaml, caching it under the same key the next request looks it up by, once per key however many requests miss together.
func (r *Resolver) readValues(ctx context.Context, name string, art *meta.Artifact, current currentDigestFunc) (Result, error) {
	values, _, err := r.downloads.do(ctx, name, func() ([]byte, error) {
		values, err := artifact.FetchValues(ctx, r.httpClient, art.URL, art.Digest, r.maxArtifactBytes)
		if err != nil {
			return nil, err
		}

		// The entry names the revision it came from, and the guard is the cache's last word before the rename: a revision that moved while this downloaded means the values are served without being kept.
		if err := r.cache.PutUnless(name, art.Digest, values, func() bool {
			return r.stillCurrent(ctx, name, current, art.Digest)
		}); err != nil {
			switch {
			case errors.Is(err, cache.ErrBudgetExhausted):
				log.FromContext(ctx).Info("not caching values.yaml, cache budget exhausted", "name", name)
			case errors.Is(err, cache.ErrSuperseded):
				// stillCurrent has already said which way it went.
			default:
				log.FromContext(ctx).Error(err, "failed to cache values.yaml")
			}
		}

		return values, nil
	})
	if err != nil {
		if errors.Is(err, artifact.ErrValuesNotFound) {
			return Result{Outcome: OutcomeValuesNotFound, Message: "chart has no values.yaml"}, nil
		}

		// A call that ended on a clock says nothing about the chart, and an http.Client timeout reads as a deadline too (since Go 1.23).
		if isTransient(err) {
			return Result{Outcome: OutcomePending}, nil
		}

		return Result{Outcome: OutcomeFetchFailed, Message: err.Error()}, nil
	}

	return Result{Outcome: OutcomeReady, Values: values}, nil
}

// stillCurrent reports whether the artifact a finished download came from is the one the source object publishes by now; an object that cannot be read at all answers no.
func (r *Resolver) stillCurrent(ctx context.Context, name string, current currentDigestFunc, downloaded string) bool {
	digest, err := current(ctx)
	if err != nil {
		log.FromContext(ctx).Info("not caching values.yaml, the source object could not be read back", "name", name, "error", err.Error())
		return false
	}

	if digest != downloaded {
		log.FromContext(ctx).Info("not caching values.yaml, the artifact revision moved while it was downloading", "name", name, "downloaded", downloaded, "current", digest)
		return false
	}

	return true
}

// isTransient reports whether an error only describes how a call ended, rather than anything about the artifact.
func isTransient(err error) bool {
	return errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, errCallAborted)
}

func (r *Resolver) ensureHelmChart(ctx context.Context, family repositoryFamily, req Request, name, expiresAt string) (*sourcev1.HelmChart, bool, error) {
	helmRepoName, err := r.findHelmRepositoryName(ctx, family, req)
	if err != nil {
		return nil, false, err
	}
	if helmRepoName == "" {
		// The backing HelmRepository is created by operator-helm-controller when it reconciles the repository.
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

// ensureOCIRepository creates or updates the auxiliary OCIRepository at the given address and tag; credentials and tls say whether the repository's own secrets describe that host.
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

// ensureHybridOCIRepository builds the source object for a version that a helm repository's index publishes in a registry, probing the media type the catalog does not record.
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
		// The operator validates the reference when it records it, so a failure here means the field was written by hand.
		return nil, &Result{Outcome: OutcomeFetchFailed, Message: err.Error()}, nil
	}

	// The repository's transport settings describe the host it names, so a registry only its index names is reached as a public one.
	sameHost := sameRegistryHost(repo.URL, url)

	mediaType := version.MediaType
	if mediaType == "" {
		var rt http.RoundTripper
		if sameHost {
			rt, err = chartartifact.Transport(repo.CACertificate, repo.InsecureSkipVerify)
			if err != nil {
				// Until the caCertificate is corrected the repository cannot be reached over TLS at all, which retrying will not change.
				return nil, &Result{Outcome: OutcomeFetchFailed, Message: err.Error()}, nil
			}
		}

		mediaType, err = r.prober.ChartLayerMediaType(ctx, version.OCIRef, rt)
		if err != nil {
			if errors.Is(err, chartartifact.ErrNotAChart) || errors.Is(err, chartartifact.ErrTagNotFound) {
				// A verdict about the artifact, reported like a version the catalog found unreadable.
				return nil, &Result{Outcome: OutcomeValuesNotFound, Message: err.Error()}, nil
			}

			// Anything else — a rate limit, a transport failure — may pass on its own.
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

// sameRegistryHost reports whether the artifact lives on the host the repository itself names; an unparsable url on either side is not the same host.
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

// findHelmRepositoryName finds the internal HelmRepository operator-helm-controller derived from this repository, selected by the source labels of the repository's kind.
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
		// The auth secret is Opaque with username/password for HelmRepository and dockerconfigjson for OCIRepository, so either key marks it.
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

// classify maps the source resource conditions and artifact to one of OutcomeReady, OutcomeFetchFailed and OutcomePending.
func classify(conditions []metav1.Condition, art *meta.Artifact) (Outcome, string) {
	if ready := apimeta.FindStatusCondition(conditions, "Ready"); ready != nil &&
		ready.Status == metav1.ConditionTrue && art != nil {
		return OutcomeReady, ""
	}

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
