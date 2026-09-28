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

package internalobj

import (
	"context"
	"fmt"
	"time"

	"github.com/fluxcd/pkg/apis/meta"
	sourcev1 "github.com/fluxcd/source-controller/api/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
	"github.com/deckhouse/operator-helm/internal/chartsource"
	repoclient "github.com/deckhouse/operator-helm/internal/client/repository"
	"github.com/deckhouse/operator-helm/internal/contract"
	"github.com/deckhouse/operator-helm/internal/utils"
)

var ociRepositoryErrorRules = []ErrorConditionRule{
	{Type: "FetchFailed", TriggerStatus: metav1.ConditionTrue, Reason: helmv1alpha1.ReasonOCIFetchFailed},
	{Type: "FetchFailed", TriggerStatus: metav1.ConditionTrue, Reason: "OCIArtifactPullFailed"},
	{Type: "IncludeUnavailable", TriggerStatus: metav1.ConditionTrue, Reason: helmv1alpha1.ReasonOCIIncludeUnavailable},
	{Type: "StorageOperationFailed", TriggerStatus: metav1.ConditionTrue, Reason: helmv1alpha1.ReasonOCIStorageFailed},
	{Type: "SourceVerified", TriggerStatus: metav1.ConditionFalse, Reason: helmv1alpha1.ReasonOCIVerificationFailed},
}

// Nothing watches a foreign registry, so this requeue is what brings a recovering one back.
const chartArtifactProbeRequeueInterval = 2 * time.Minute

type OCIRepositories struct {
	client          client.Client
	targetNamespace string
	resolver        repoclient.ChartResolverInterface
}

// NewOCIRepositories builds the gateway. A nil resolver selects the default one.
func NewOCIRepositories(
	client client.Client,
	namespace string,
	resolver repoclient.ChartResolverInterface,
) *OCIRepositories {
	if resolver == nil {
		resolver = repoclient.OCIChartResolverDefault
	}

	return &OCIRepositories{
		client:          client,
		targetNamespace: namespace,
		resolver:        resolver,
	}
}

func (s *OCIRepositories) Ensure(
	ctx context.Context,
	rel contract.Release,
	repo contract.Repository,
	src chartsource.Source,
	version *helmv1alpha1.ChartVersion,
) OCIRepoOutcome {
	logger := log.FromContext(ctx)

	mediaType, failure := s.resolveMediaType(ctx, rel, repo, src, version)
	if failure != nil {
		return *failure
	}

	existing := &sourcev1.OCIRepository{
		ObjectMeta: metav1.ObjectMeta{
			Name:      rel.InternalNames().OCIRepository,
			Namespace: s.targetNamespace,
		},
	}

	op, err := controllerutil.CreateOrPatch(ctx, s.client, existing, func() error {
		applyOCIRepositorySpec(rel, repo, src, mediaType, existing)

		return nil
	})
	if err != nil {
		return OCIRepoOutcome{Err: fmt.Errorf("creating oci repository: %w", err)}
	}

	if op != controllerutil.OperationResultNone {
		logger.Info("Reconciled oci repository", "operation", op,
			"internalObject", client.ObjectKeyFromObject(existing))
	}

	internal := reduceInternalConditions(existing.Status.Conditions, existing.Generation, ociRepositoryErrorRules)

	return OCIRepoOutcome{
		Artifact:       existing.Status.Artifact,
		Internal:       internal,
		VersionRemoved: version.UnavailableReason == helmv1alpha1.UnavailableReasonRemovedFromRepository,
		Version:        version.Version,
		RepositoryName: repo.Name(),
	}
}

func (s *OCIRepositories) resolveMediaType(
	ctx context.Context,
	rel contract.Release,
	repo contract.Repository,
	src chartsource.Source,
	version *helmv1alpha1.ChartVersion,
) (string, *OCIRepoOutcome) {
	if version.MediaType != "" {
		return version.MediaType, nil
	}

	// The internal OCIRepository caches the verdict; a force request asks for a fresh one.
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
		return "", &OCIRepoOutcome{
			ProbeErr:      err,
			ProbeReason:   terminal.Reason,
			ProbeMessage:  terminal.Message,
			ProbeTerminal: true,
		}
	}

	return "", &OCIRepoOutcome{
		ProbeErr:          err,
		ProbeReason:       helmv1alpha1.ReasonOCIFetchFailed,
		ProbeMessage:      "Failed to examine the chart artifact: " + err.Error(),
		ProbeRequeueAfter: chartArtifactProbeRequeueInterval,
	}
}

func (s *OCIRepositories) cachedMediaType(
	ctx context.Context,
	names contract.ReleaseNames,
	src chartsource.Source,
) string {
	nn := types.NamespacedName{
		Name:      names.OCIRepository,
		Namespace: s.targetNamespace,
	}

	existing := &sourcev1.OCIRepository{}
	if err := s.client.Get(ctx, nn, existing); err != nil {
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

func artifactRepoConfig(repo contract.Repository, src chartsource.Source) *repoclient.RepoConfig {
	// Transport settings only, and only for the host the repository names: the pull is anonymous.
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

// Remove issues a delete for the internal OCIRepository and returns it while it is still
// present, so the caller can wait for source-controller to finish removing it.
func (s *OCIRepositories) Remove(ctx context.Context, names contract.ReleaseNames) (*sourcev1.OCIRepository, error) {
	nn := types.NamespacedName{Name: names.OCIRepository, Namespace: s.targetNamespace}
	ociRepo := &sourcev1.OCIRepository{}
	exists, err := utils.DeleteIfPresent(ctx, s.client, nn, ociRepo)
	if err != nil {
		return nil, fmt.Errorf("removing oci repository: %w", err)
	}
	if !exists {
		return nil, nil
	}

	return ociRepo, nil
}

func applyOCIRepositorySpec(
	rel contract.Release,
	repo contract.Repository,
	src chartsource.Source,
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

	// An artifact the index points at another host is reached as a public registry, with no credentials.
	if sameRegistryHost(repo.URL(), src.URL) {
		existing.Spec.Insecure = repo.InsecureSkipVerify()

		if repo.CACertificate() != "" {
			existing.Spec.CertSecretRef = &meta.LocalObjectReference{
				Name: names.TLSSecret,
			}
		}

		// Only an oci:// repository keeps its credentials in the dockerconfigjson secret this needs.
		if repo.Auth() != nil && repositoryIsOCI(repo) {
			existing.Spec.SecretRef = &meta.LocalObjectReference{
				Name: names.AuthSecret,
			}
		}
	}

	// The caller guarantees the media type is non-empty.
	existing.Spec.LayerSelector = &sourcev1.OCILayerSelector{
		MediaType: mediaType,
		Operation: "copy",
	}

	applyOwnedLabels(existing, rel.SourceLabels())
}

func sameRegistryHost(repoURL, artifactURL string) bool {
	// An unparsable url on either side answers "not the same host", which withholds credentials.
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

func repositoryIsOCI(repo contract.Repository) bool {
	repoType, err := chartsource.KindOf(repo.URL())

	return err == nil && repoType == chartsource.OCI
}
