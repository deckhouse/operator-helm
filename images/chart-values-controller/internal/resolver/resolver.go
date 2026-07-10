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
	"strings"
	"time"

	"github.com/werf/3p-fluxcd-pkg/apis/meta"
	sourcev1 "github.com/werf/nelm-source-controller/api/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/deckhouse/chart-values-controller/internal/artifact"
	"github.com/deckhouse/chart-values-controller/internal/cache"
	"github.com/deckhouse/chart-values-controller/internal/labels"
	"github.com/deckhouse/chart-values-controller/internal/naming"
	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
)

// helmChartLayerMediaType is the OCI media type of the layer that holds a
// packaged Helm chart.
const helmChartLayerMediaType = "application/vnd.cncf.helm.chart.content.v1.tar+gzip"

// Outcome enumerates the possible results of resolving a chart-values request.
type Outcome string

const (
	OutcomeReady              Outcome = "ready"
	OutcomePending            Outcome = "pending"
	OutcomeRepositoryNotFound Outcome = "repository_not_found"
	OutcomeFetchFailed        Outcome = "fetch_failed"
	OutcomeValuesNotFound     Outcome = "values_not_found"
)

// Request identifies a chart by repository, name and version.
type Request struct {
	Repository string
	Chart      string
	Version    string
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
	}
}

// Resolve ensures the auxiliary source resource for req exists, inspects its
// status and, when the artifact is ready, returns the chart's values.yaml.
// A returned error indicates an internal failure (HTTP 500); all expected
// states are reported via Result.Outcome.
func (r *Resolver) Resolve(ctx context.Context, req Request) (Result, error) {
	name := naming.AuxResourceName(req.Repository, req.Chart, req.Version)

	// Fast path: the cache (keyed by the auxiliary resource name) is kept fresh by
	// the auxiliary-resource controller via a watch with a revision-change predicate,
	// so a hit is served without any Kubernetes calls.
	if values, ok := r.cache.Get(name); ok {
		return Result{Outcome: OutcomeReady, Values: values}, nil
	}

	repo := &helmv1alpha1.HelmClusterAddonRepository{}
	if err := r.client.Get(ctx, types.NamespacedName{Name: req.Repository}, repo); err != nil {
		if apierrors.IsNotFound(err) {
			return Result{Outcome: OutcomeRepositoryNotFound, Message: fmt.Sprintf("repository %q not found", req.Repository)}, nil
		}
		return Result{}, fmt.Errorf("getting repository: %w", err)
	}

	expiresAt := time.Now().UTC().Add(r.ttl).Format(time.RFC3339)

	var conditions []metav1.Condition
	var art *meta.Artifact

	switch {
	case isOCI(repo.Spec.URL):
		ociRepo, err := r.ensureOCIRepository(ctx, repo, req, expiresAt)
		if err != nil {
			return Result{}, err
		}
		conditions, art = ociRepo.Status.Conditions, ociRepo.Status.Artifact
	case isHelm(repo.Spec.URL):
		chart, pending, err := r.ensureHelmChart(ctx, repo, req, expiresAt)
		if err != nil {
			return Result{}, err
		}
		if pending {
			return Result{Outcome: OutcomePending}, nil
		}
		conditions, art = chart.Status.Conditions, chart.Status.Artifact
	default:
		return Result{Outcome: OutcomeFetchFailed, Message: fmt.Sprintf("unsupported repository URL scheme: %q", repo.Spec.URL)}, nil
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

func (r *Resolver) ensureHelmChart(ctx context.Context, repo *helmv1alpha1.HelmClusterAddonRepository, req Request, expiresAt string) (*sourcev1.HelmChart, bool, error) {
	helmRepoName, err := r.findHelmRepositoryName(ctx, repo.Name)
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
			Name:      naming.AuxResourceName(req.Repository, req.Chart, req.Version),
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

func (r *Resolver) ensureOCIRepository(ctx context.Context, repo *helmv1alpha1.HelmClusterAddonRepository, req Request, expiresAt string) (*sourcev1.OCIRepository, error) {
	authSecret, tlsSecret, err := r.findRepositorySecretNames(ctx, repo.Name)
	if err != nil {
		return nil, err
	}

	ociRepo := &sourcev1.OCIRepository{
		ObjectMeta: metav1.ObjectMeta{
			Name:      naming.AuxResourceName(req.Repository, req.Chart, req.Version),
			Namespace: r.namespace,
		},
	}

	if _, err := controllerutil.CreateOrPatch(ctx, r.client, ociRepo, func() error {
		applyManagedMeta(ociRepo, expiresAt)
		ociRepo.Spec.URL = repo.Spec.URL
		ociRepo.Spec.Reference = &sourcev1.OCIRepositoryRef{Tag: req.Version}
		ociRepo.Spec.Interval = r.sourceInterval
		ociRepo.Spec.Insecure = repo.Spec.InsecureSkipVerify
		ociRepo.Spec.LayerSelector = &sourcev1.OCILayerSelector{
			MediaType: helmChartLayerMediaType,
			Operation: "copy",
		}

		ociRepo.Spec.SecretRef = nil
		ociRepo.Spec.CertSecretRef = nil
		if repo.Spec.Auth != nil && authSecret != "" {
			ociRepo.Spec.SecretRef = &meta.LocalObjectReference{Name: authSecret}
		}
		if repo.Spec.CACertificate != "" && tlsSecret != "" {
			ociRepo.Spec.CertSecretRef = &meta.LocalObjectReference{Name: tlsSecret}
		}

		return nil
	}); err != nil {
		return nil, fmt.Errorf("ensuring oci repository: %w", err)
	}

	return ociRepo, nil
}

func (r *Resolver) findHelmRepositoryName(ctx context.Context, repoName string) (string, error) {
	var list sourcev1.HelmRepositoryList
	if err := r.client.List(
		ctx, &list,
		client.InNamespace(r.namespace),
		client.MatchingLabels{helmv1alpha1.HelmClusterAddonRepositoryLabelSourceName: repoName},
	); err != nil {
		return "", fmt.Errorf("listing helm repositories: %w", err)
	}

	if len(list.Items) == 0 {
		return "", nil
	}

	return list.Items[0].Name, nil
}

func (r *Resolver) findRepositorySecretNames(ctx context.Context, repoName string) (auth, tls string, err error) {
	var list corev1.SecretList
	if err := r.client.List(
		ctx, &list,
		client.InNamespace(r.namespace),
		client.MatchingLabels{helmv1alpha1.HelmClusterAddonRepositoryLabelSourceName: repoName},
	); err != nil {
		return "", "", fmt.Errorf("listing repository secrets: %w", err)
	}

	for i := range list.Items {
		secret := &list.Items[i]
		if _, ok := secret.Data["username"]; ok {
			auth = secret.Name
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
