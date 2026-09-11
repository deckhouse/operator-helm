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

package services

import (
	"context"
	"fmt"
	"time"

	"github.com/werf/3p-fluxcd-pkg/apis/meta"
	sourcev1 "github.com/werf/nelm-source-controller/api/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
	repoclient "github.com/deckhouse/operator-helm/internal/client/repository"
	"github.com/deckhouse/operator-helm/internal/manager/status"
	"github.com/deckhouse/operator-helm/internal/source"
	"github.com/deckhouse/operator-helm/internal/utils"
)

var ociRepositoryErrorRules = []status.ErrorConditionRule{
	{Type: "FetchFailed", TriggerStatus: metav1.ConditionTrue, Reason: helmv1alpha1.ReasonOCIFetchFailed},
	{Type: "FetchFailed", TriggerStatus: metav1.ConditionTrue, Reason: "OCIArtifactPullFailed"},
	{Type: "IncludeUnavailable", TriggerStatus: metav1.ConditionTrue, Reason: helmv1alpha1.ReasonOCIIncludeUnavailable},
	{Type: "StorageOperationFailed", TriggerStatus: metav1.ConditionTrue, Reason: helmv1alpha1.ReasonOCIStorageFailed},
	{Type: "SourceVerified", TriggerStatus: metav1.ConditionFalse, Reason: helmv1alpha1.ReasonOCIVerificationFailed},
}

// chartArtifactProbeRequeueInterval bounds how often a version whose artifact could
// not be examined is retried. There is no watch that fires when a registry starts
// answering again, so this periodic requeue is what lets a rate-limited or briefly
// unreachable registry recover on its own.
const chartArtifactProbeRequeueInterval = 2 * time.Minute

type OCIRepoService struct {
	BaseRepoService

	resolver repoclient.ChartResolverInterface
}

// NewOCIRepoService builds the service. A nil resolver selects the default one; tests
// pass their own so they never reach a registry.
func NewOCIRepoService(
	client client.Client,
	scheme *runtime.Scheme,
	namespace string,
	resolver repoclient.ChartResolverInterface,
) *OCIRepoService {
	if resolver == nil {
		resolver = repoclient.OCIChartResolverDefault
	}

	return &OCIRepoService{
		BaseRepoService: BaseRepoService{
			BaseService: BaseService{
				Client: client,
				Scheme: scheme,
			},
			TargetNamespace: namespace,
		},
		resolver: resolver,
	}
}

var _ status.Provider = (*OCIRepoResult)(nil)

type OCIRepoResult struct {
	Status   status.Status
	Artifact *meta.Artifact
	// RequeueAfter asks the caller to schedule another pass. It is set only when the
	// artifact could not be examined for a reason that may pass on its own: there is
	// no watch on a foreign registry.
	RequeueAfter time.Duration
}

func (r OCIRepoResult) GetStatus() status.Status {
	return r.Status
}

func (r OCIRepoResult) IsReady() bool {
	return r.Artifact != nil && r.Status.Observed && r.Status.Status == metav1.ConditionTrue
}

func (r OCIRepoResult) HasArtifact() bool {
	return r.Artifact != nil && r.Status.Observed
}

func (r OCIRepoResult) GetConditionType() string {
	return helmv1alpha1.ConditionTypeReady
}

func (s *OCIRepoService) EnsureInternalOCIRepository(
	ctx context.Context,
	rel source.Release,
	repo source.Repository,
	src utils.ChartSource,
	version *helmv1alpha1.ChartVersion,
) OCIRepoResult {
	logger := log.FromContext(ctx)

	mediaType, failure := s.resolveMediaType(ctx, rel, repo, src, version)
	if failure != nil {
		return *failure
	}

	existing := &sourcev1.OCIRepository{
		ObjectMeta: metav1.ObjectMeta{
			Name:      rel.InternalNames().OCIRepository,
			Namespace: s.TargetNamespace,
		},
	}

	op, err := controllerutil.CreateOrPatch(ctx, s.Client, existing, func() error {
		applyOCIRepositorySpec(rel, repo, src, mediaType, existing)

		return nil
	})
	if err != nil {
		return OCIRepoResult{
			Status: status.Failed(
				rel.Object(),
				helmv1alpha1.ReasonFailed,
				"Failed to reconcile oci repository",
				fmt.Errorf("creating oci repository: %w", err),
			),
		}
	}

	if op != controllerutil.OperationResultNone {
		logger.Info("Reconciled oci repository", "operation", op)
	}

	processedStatus := status.ProcessChildConditions(
		existing.Status.Conditions, existing.Generation, rel.Object(), ociRepositoryErrorRules,
	)

	if version.UnavailableReason == helmv1alpha1.UnavailableReasonRemovedFromRepository &&
		processedStatus.Status != metav1.ConditionTrue {
		// The version is still recorded — that is what keeps this addon reconcilable —
		// but the repository no longer offers the tag, so the pull cannot succeed. Name
		// that cause instead of leaving only the source controller's "not found".
		processedStatus.Reason = helmv1alpha1.ReasonChartVersionRemoved
		processedStatus.Message = fmt.Sprintf(
			"Version %s is no longer offered by repository %s: %s",
			version.Version, repo.Name(), processedStatus.Message,
		)
	}

	return OCIRepoResult{
		Artifact: existing.Status.Artifact,
		Status:   processedStatus,
	}
}

// resolveMediaType decides the layer selector for this version. A version of an
// oci:// repository carries the verdict the catalog reached for it. A version a helm
// index publishes in a registry does not: the catalog records only the reference, and
// the artifact is examined here, when the addon is about to be deployed.
//
// The internal OCIRepository is the cache of that verdict, and it needs no upkeep of
// its own: it lives exactly as long as the addon/version pair it serves, and it
// already records which artifact the verdict is about. The registry is asked only
// when the object is missing, addresses a different artifact, or carries no selector
// — or when a force request asks for a re-examination.
func (s *OCIRepoService) resolveMediaType(
	ctx context.Context,
	rel source.Release,
	repo source.Repository,
	src utils.ChartSource,
	version *helmv1alpha1.ChartVersion,
) (string, *OCIRepoResult) {
	if version.MediaType != "" {
		return version.MediaType, nil
	}

	if !rel.ForceReconcileRequired() {
		if cached := s.cachedMediaType(ctx, rel.InternalNames(), src); cached != "" {
			return cached, nil
		}
	}

	mediaType, err := s.resolver.ResolveChartArtifact(ctx, src.URL+":"+src.Tag, artifactRepoConfig(repo, src))
	if err == nil {
		return mediaType, nil
	}

	if terminal, ok := repoclient.AsTerminal(err); ok {
		return "", &OCIRepoResult{
			Status: status.Failed(rel.Object(), terminal.Reason, terminal.Message, err),
		}
	}

	return "", &OCIRepoResult{
		Status: status.Failed(
			rel.Object(),
			helmv1alpha1.ReasonOCIFetchFailed,
			"Failed to examine the chart artifact: "+err.Error(),
			err,
		),
		RequeueAfter: chartArtifactProbeRequeueInterval,
	}
}

// cachedMediaType returns the verdict recorded on the internal object, but only if it
// is a verdict about this exact artifact.
func (s *OCIRepoService) cachedMediaType(
	ctx context.Context,
	names source.ReleaseNames,
	src utils.ChartSource,
) string {
	nn := types.NamespacedName{
		Name:      names.OCIRepository,
		Namespace: s.TargetNamespace,
	}

	existing := &sourcev1.OCIRepository{}
	if err := s.Client.Get(ctx, nn, existing); err != nil {
		return ""
	}

	if existing.Spec.URL != src.URL {
		return ""
	}
	if existing.Spec.Reference == nil || existing.Spec.Reference.Tag != src.Tag {
		return ""
	}

	return existing.GetLayerMediaType()
}

// artifactRepoConfig builds the transport settings for examining the artifact. Only
// the host the repository names gets them, and credentials are never included: the
// internal OCIRepository pulls a foreign registry anonymously, and a probe that
// authenticated would report a chart the pull could not fetch.
func artifactRepoConfig(repo source.Repository, src utils.ChartSource) *repoclient.RepoConfig {
	if !sameRegistryHost(repo.URL(), src.URL) {
		return nil
	}

	if repo.CACertificate() == "" && !repo.InsecureSkipVerify() {
		return nil
	}

	return &repoclient.RepoConfig{
		CACertificate: repo.CACertificate(),
		Insecure:      repo.InsecureSkipVerify(),
	}
}

func (s *OCIRepoService) CleanupOCIRepository(ctx context.Context, names source.InternalNames) error {
	for _, name := range []string{names.AuthSecret, names.TLSSecret} {
		nn := types.NamespacedName{Name: name, Namespace: s.TargetNamespace}
		if err := s.ensureResourceDeleted(ctx, nn, &corev1.Secret{}); err != nil {
			return fmt.Errorf("cleaning up secret %s: %w", name, err)
		}
	}

	return nil
}

// RemoveOCIRepository issues a delete for the internal OCIRepository and returns
// it while it is still present, so the caller can inspect its conditions and wait
// for nelm-source-controller to finish removing it. It returns nil once the
// OCIRepository is gone.
func (s *OCIRepoService) RemoveOCIRepository(ctx context.Context, names source.ReleaseNames) (*sourcev1.OCIRepository, error) {
	nn := types.NamespacedName{Name: names.OCIRepository, Namespace: s.TargetNamespace}
	ociRepo := &sourcev1.OCIRepository{}
	exists, err := s.deleteAndCheck(ctx, nn, ociRepo)
	if err != nil {
		return nil, fmt.Errorf("removing oci repository: %w", err)
	}
	if !exists {
		return nil, nil
	}

	return ociRepo, nil
}

func applyOCIRepositorySpec(
	rel source.Release,
	repo source.Repository,
	src utils.ChartSource,
	mediaType string,
	existing *sourcev1.OCIRepository,
) {
	if rel.ForceReconcileRequired() {
		setReconcileRequestAnnotations(existing)
	}

	existing.Spec.URL = src.URL
	existing.Spec.Reference = &sourcev1.OCIRepositoryRef{Tag: src.Tag}
	existing.Spec.Interval = metav1.Duration{Duration: InternalRepositoryInterval}
	existing.Spec.Insecure = false
	existing.Spec.CertSecretRef = nil
	existing.Spec.SecretRef = nil

	names := repo.InternalNames()

	// The repository's transport settings and credentials describe the host it
	// names. An artifact its index points at somewhere else is reached as a public
	// registry: the settings do not describe that host, and the credentials must not
	// be sent to it. For an oci:// repository the two hosts are the same one, so this
	// is where its existing behaviour lives.
	if sameRegistryHost(repo.URL(), src.URL) {
		existing.Spec.Insecure = repo.InsecureSkipVerify()

		if repo.CACertificate() != "" {
			existing.Spec.CertSecretRef = &meta.LocalObjectReference{
				Name: names.TLSSecret,
			}
		}

		// Only an oci:// repository keeps its credentials in the dockerconfigjson
		// secret OCIRepository requires. A helm repository's secret is an Opaque
		// username/password one, and referencing it here would break the pull with a
		// less obvious error than not authenticating at all.
		if repo.Auth() != nil && repositoryIsOCI(repo) {
			existing.Spec.SecretRef = &meta.LocalObjectReference{
				Name: names.AuthSecret,
			}
		}
	}

	// The media type is either the one recorded for this version by the repository
	// synchronization or the one this pass read from the registry: it differs between
	// charts pushed by current and by older tooling. The caller guarantees it is
	// non-empty.
	existing.Spec.LayerSelector = &sourcev1.OCILayerSelector{
		MediaType: mediaType,
		Operation: "copy",
	}

	existing.Labels = rel.SourceLabels()
}

// sameRegistryHost reports whether the artifact lives on the host the repository
// itself names. An unparsable url on either side means "not the same host", which is
// the safe answer: it withholds credentials rather than misdirecting them.
func sameRegistryHost(repoURL, artifactURL string) bool {
	repoHost, err := utils.GetRegistryHost(repoURL)
	if err != nil {
		return false
	}

	artifactHost, err := utils.GetRegistryHost(artifactURL)
	if err != nil {
		return false
	}

	return repoHost == artifactHost
}

func repositoryIsOCI(repo source.Repository) bool {
	repoType, err := utils.GetRepositoryType(repo.URL())

	return err == nil && repoType == utils.InternalOCIRepository
}
