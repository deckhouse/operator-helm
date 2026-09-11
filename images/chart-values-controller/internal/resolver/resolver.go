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

	"github.com/werf/3p-fluxcd-pkg/apis/meta"
	sourcev1 "github.com/werf/nelm-source-controller/api/v1"
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
// repository kinds are added as new constants plus a case in Resolve.
type RepositoryKind string

const (
	// RepositoryKindHelmClusterAddon is a chart referenced by a
	// HelmClusterAddonRepository custom resource. Kind values are stored and
	// compared in lower case, so the request casing does not matter.
	RepositoryKindHelmClusterAddon RepositoryKind = "helmclusteraddonrepository"

	// RepositoryKindHelmApplication is a chart referenced by a
	// HelmApplicationRepository, the namespaced repository of the application
	// family: a request for it must name the namespace.
	RepositoryKindHelmApplication RepositoryKind = "helmapplicationrepository"

	// RepositoryKindHelmClusterApplication is a chart referenced by a
	// HelmClusterApplicationRepository, the cluster-wide repository of the
	// application family.
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
	// kind it names — a namespaced kind without a namespace, or the reverse.
	OutcomeInvalidRequest Outcome = "invalid_request"
)

// Request identifies a chart by repository kind, repository namespace, repository
// name, chart name and chart version. Namespace is empty for a cluster-scoped
// repository kind.
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

// Resolve dispatches on the repository kind and, when the artifact is ready,
// returns the chart's values.yaml. A returned error indicates an internal
// failure (HTTP 500); all expected states are reported via Result.Outcome.
func (r *Resolver) Resolve(ctx context.Context, req Request) (Result, error) {
	// Kind is case-insensitive: normalize to lower case so it dispatches and
	// contributes to the resource name hash consistently. The original casing is
	// kept only for the error message.
	requested := req.Kind
	req.Kind = RepositoryKind(strings.ToLower(string(req.Kind)))

	family, ok := familyFor(req.Kind)
	if !ok {
		return Result{Outcome: OutcomeUnsupportedRepositoryKind, Message: fmt.Sprintf("unsupported repository kind %q", requested)}, nil
	}

	if err := family.requireNamespace(req.Namespace); err != nil {
		return Result{Outcome: OutcomeInvalidRequest, Message: err.Error()}, nil
	}

	return r.resolveChart(ctx, family, req)
}

// chartVersion finds the catalog entry for the requested version. It reports only
// whether the entry exists: what the entry means differs per repository kind, and
// judging it here is what made every archive version look unresolved.
//
// A missing chart object or a missing version is pending. The catalog is what says
// where a version lives, and choosing a source without it would mean falling back to
// guessing from the repository url — the very thing that sends a version published in
// a registry down the HTTP path.
//
// A non-nil Result means the caller must stop and return it.
func (r *Resolver) chartVersion(ctx context.Context, family repositoryFamily, req Request) (*helmv1alpha1.ChartVersion, *Result, error) {
	versions, err := family.ChartVersions(ctx, r.client, req.Namespace, req.RepositoryName, req.Chart)
	if err != nil {
		if apierrors.IsNotFound(err) {
			// The chart object is created by operator-helm-controller when it synchronizes
			// the repository: until then the catalog simply has not caught up.
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
			// The index points this version at a registry with a reference that cannot
			// be addressed. Left through it would fall to the archive path and fail on
			// the very same url, reported by the source controller as an opaque fetch
			// error.
			return nil, &Result{
				Outcome: OutcomeValuesNotFound,
				Message: fmt.Sprintf("chart version %s is not readable (%s)", req.Version, versionDetail(version)),
			}, nil
		}

		return version, nil, nil
	}

	return nil, &Result{Outcome: OutcomePending}, nil
}

// ociMediaType reports the layer media type the catalog recorded for a version of an
// oci:// repository. Such a version is only readable once the catalog has reached a
// verdict on it, so an empty media type is a state rather than a value.
//
// A non-nil Result means the caller must stop and return it.
func ociMediaType(req Request, version *helmv1alpha1.ChartVersion) (string, *Result) {
	if version.MediaType != "" {
		return version.MediaType, nil
	}

	if version.UnavailableReason == helmv1alpha1.UnavailableReasonResolvePending || version.UnavailableReason == "" {
		// Both an explicit ResolvePending and an empty reason mean the catalog has not
		// reached a verdict yet, so the caller should retry rather than being told the
		// version is permanently unreadable. An empty reason alongside an empty media
		// type is the pre-upgrade shape of a version entry (written before verdicts
		// were recorded at all): the operator re-resolves it on its next normal
		// synchronization, exactly like ResolvePending.
		return "", &Result{Outcome: OutcomePending}
	}

	// Every other reason is a durable verdict that will not change without a change in
	// the repository (an unsupported media type, or a removed tag with no media type on
	// record), so it is reported as values-not-found, naming why.
	return "", &Result{
		Outcome: OutcomeValuesNotFound,
		Message: fmt.Sprintf("chart version %s is not readable (%s)", req.Version, versionDetail(version)),
	}
}

// versionDetail renders why a catalog entry is unusable.
func versionDetail(version *helmv1alpha1.ChartVersion) string {
	detail := version.UnavailableReason
	if version.UnavailableMessage != "" {
		detail += ": " + version.UnavailableMessage
	}

	return detail
}

// resolveChart ensures the auxiliary source resource for one chart exists, inspects
// its status and returns the chart's values.yaml once the artifact is ready. Every
// repository kind takes this path; what differs — where the repository and its
// catalog are read from, and how its internal objects are recognised — arrives in
// the family.
func (r *Resolver) resolveChart(ctx context.Context, family repositoryFamily, req Request) (Result, error) {
	name := naming.AuxResourceName(string(req.Kind), req.Namespace, req.RepositoryName, req.Chart, req.Version)

	// Fast path: the cache (keyed by the auxiliary resource name) is kept fresh by
	// the auxiliary-resource controller via a watch with a revision-change predicate,
	// so a hit is served without any Kubernetes calls.
	if values, ok := r.cache.Get(name); ok {
		return Result{Outcome: OutcomeReady, Values: values}, nil
	}

	repo, err := family.GetRepository(ctx, r.client, req.Namespace, req.RepositoryName)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return Result{Outcome: OutcomeRepositoryNotFound, Message: fmt.Sprintf("repository %q not found", req.RepositoryName)}, nil
		}
		return Result{}, fmt.Errorf("getting repository: %w", err)
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

	switch {
	case version.OCIRef != "":
		// The index publishes this version in a registry, whatever the repository's
		// own url scheme is. Reading it through a HelmChart would make the source
		// controller download the index url over HTTP and fail on the oci:// scheme.
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

	switch outcome, message := classify(conditions, art); outcome {
	case OutcomeFetchFailed:
		return Result{Outcome: OutcomeFetchFailed, Message: message}, nil
	case OutcomePending:
		return Result{Outcome: OutcomePending}, nil
	}

	return r.readValues(ctx, name, art)
}

func (r *Resolver) readValues(ctx context.Context, name string, art *meta.Artifact) (Result, error) {
	values, err := artifact.FetchValues(ctx, r.httpClient, art.URL, art.Digest, r.maxArtifactBytes)
	if err != nil {
		if errors.Is(err, artifact.ErrValuesNotFound) {
			return Result{Outcome: OutcomeValuesNotFound, Message: "chart has no values.yaml"}, nil
		}
		return Result{Outcome: OutcomeFetchFailed, Message: err.Error()}, nil
	}

	if err := r.cache.Put(name, values); err != nil {
		log.FromContext(ctx).Error(err, "failed to cache values.yaml")
	}

	return Result{Outcome: OutcomeReady, Values: values}, nil
}

func (r *Resolver) ensureHelmChart(ctx context.Context, family repositoryFamily, req Request, name, expiresAt string) (*sourcev1.HelmChart, bool, error) {
	helmRepoName, err := r.findHelmRepositoryName(ctx, family, req)
	if err != nil {
		return nil, false, err
	}
	if helmRepoName == "" {
		// The backing HelmRepository is created by operator-helm-controller when
		// it reconciles the HelmClusterAddonRepository; until then, wait.
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

// ensureOCIRepository creates or updates the auxiliary OCIRepository. The address and
// tag are passed in rather than derived from the repository, because a version its
// index publishes elsewhere lives at a different address entirely. credentials and tls
// say whether the repository's own secrets describe the host being addressed.
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

// ensureHybridOCIRepository builds the source object for a version that a helm
// repository's index publishes in a registry. Its media type is not in the catalog by
// design, so it is probed here; the probe is the only registry call this service makes
// and its answer is then carried by the source object's layer selector.
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
		// The reference was validated when the operator recorded it, so a failure here
		// means the field was written by hand or by an older version.
		return nil, &Result{Outcome: OutcomeFetchFailed, Message: err.Error()}, nil
	}

	// The repository's transport settings describe the host it names. A registry only
	// its index names is reached as a public one, and its credentials are never sent
	// there.
	sameHost := sameRegistryHost(repo.URL, url)

	mediaType := version.MediaType
	if mediaType == "" {
		var rt http.RoundTripper
		if sameHost {
			rt = chartartifact.Transport(repo.CACertificate, repo.InsecureSkipVerify)
		}

		mediaType, err = r.prober.ChartLayerMediaType(ctx, version.OCIRef, rt)
		if err != nil {
			if errors.Is(err, chartartifact.ErrNotAChart) || errors.Is(err, chartartifact.ErrTagNotFound) {
				// A verdict about the artifact: retrying cannot change it, so it is
				// reported the same way as a version the catalog found unreadable.
				return nil, &Result{Outcome: OutcomeValuesNotFound, Message: err.Error()}, nil
			}

			// Anything else may pass on its own — a rate limit, a transport failure —
			// so the client is told to retry rather than that the values do not exist.
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

// sameRegistryHost reports whether the artifact lives on the host the repository
// itself names. An unparsable url on either side means "not the same host", which is
// the safe answer: it withholds settings rather than misapplying them.
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

// findHelmRepositoryName finds the internal HelmRepository operator-helm-controller
// derived from this repository. It is selected by the source labels of the
// repository's own kind: internal objects of every family live in one namespace, so
// the labels are what tell them apart.
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
		// The auth secret is Opaque with username/password for HelmRepository and
		// kubernetes.io/dockerconfigjson for OCIRepository, so either key marks it.
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

// classify maps the source resource conditions and artifact to an outcome.
// Only OutcomeReady, OutcomeFetchFailed and OutcomePending are returned here.
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
