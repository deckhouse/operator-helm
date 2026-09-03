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

	"github.com/werf/3p-fluxcd-pkg/apis/meta"
	sourcev1 "github.com/werf/nelm-source-controller/api/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
	"github.com/deckhouse/operator-helm/internal/index"
	"github.com/deckhouse/operator-helm/internal/manager/status"
	"github.com/deckhouse/operator-helm/internal/utils"
)

var ociRepositoryErrorRules = []status.ErrorConditionRule{
	{Type: "FetchFailed", TriggerStatus: metav1.ConditionTrue, Reason: helmv1alpha1.ReasonOCIFetchFailed},
	{Type: "FetchFailed", TriggerStatus: metav1.ConditionTrue, Reason: "OCIArtifactPullFailed"},
	{Type: "IncludeUnavailable", TriggerStatus: metav1.ConditionTrue, Reason: helmv1alpha1.ReasonOCIIncludeUnavailable},
	{Type: "StorageOperationFailed", TriggerStatus: metav1.ConditionTrue, Reason: helmv1alpha1.ReasonOCIStorageFailed},
	{Type: "SourceVerified", TriggerStatus: metav1.ConditionFalse, Reason: helmv1alpha1.ReasonOCIVerificationFailed},
}

type OCIRepoService struct {
	BaseRepoService
}

func NewOCIRepoService(client client.Client, scheme *runtime.Scheme, namespace string) *OCIRepoService {
	return &OCIRepoService{
		BaseRepoService: BaseRepoService{
			BaseService: BaseService{
				Client: client,
				Scheme: scheme,
			},
			TargetNamespace: namespace,
		},
	}
}

var _ status.Provider = (*OCIRepoResult)(nil)

type OCIRepoResult struct {
	Status   status.Status
	Artifact *meta.Artifact
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
	addon *helmv1alpha1.HelmClusterAddon,
	repo *helmv1alpha1.HelmClusterAddonRepository,
	version *helmv1alpha1.HelmClusterAddonChartVersion,
) OCIRepoResult {
	logger := log.FromContext(ctx)

	existing := &sourcev1.OCIRepository{
		ObjectMeta: metav1.ObjectMeta{
			Name:      utils.GetInternalOCIRepositoryName(addon.Name),
			Namespace: s.TargetNamespace,
		},
	}

	op, err := controllerutil.CreateOrPatch(ctx, s.Client, existing, func() error {
		applyOCIRepositorySpec(addon, repo, version.MediaType, existing)

		return nil
	})
	if err != nil {
		return OCIRepoResult{
			Status: status.Failed(
				addon,
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
		existing.Status.Conditions, existing.Generation, addon, ociRepositoryErrorRules,
	)

	if version.UnavailableReason == helmv1alpha1.UnavailableReasonRemovedFromRepository &&
		processedStatus.Status != metav1.ConditionTrue {
		// The version is still recorded — that is what keeps this addon reconcilable —
		// but the repository no longer offers the tag, so the pull cannot succeed. Name
		// that cause instead of leaving only the source controller's "not found".
		processedStatus.Reason = helmv1alpha1.ReasonChartVersionRemoved
		processedStatus.Message = fmt.Sprintf(
			"Version %s is no longer offered by repository %s: %s",
			version.Version, repo.Name, processedStatus.Message,
		)
	}

	return OCIRepoResult{
		Artifact: existing.Status.Artifact,
		Status:   processedStatus,
	}
}

// ForceReconcileInternalRepositories stamps the reconcile request annotations on
// the internal OCIRepository of every addon that references repoName.
//
// An oci:// repository has no internal source object of its own: the artifact is
// pulled per addon, so a force request on the repository reaches the artifacts
// only through its addons' OCIRepositories. The helm:// path needs no equivalent -
// there the internal HelmRepository carries the request and its HelmCharts follow
// the re-indexed source on their own.
//
// An addon whose internal OCIRepository does not exist yet is skipped: the force
// request must not be blocked by an addon that has not reached the point of
// building one.
func (s *OCIRepoService) ForceReconcileInternalRepositories(ctx context.Context, repoName string) error {
	addons := &helmv1alpha1.HelmClusterAddonList{}
	if err := s.Client.List(ctx, addons, client.MatchingFields{index.AddonRepository: repoName}); err != nil {
		return fmt.Errorf("listing addons of repository %s: %w", repoName, err)
	}

	for i := range addons.Items {
		name := utils.GetInternalOCIRepositoryName(addons.Items[i].Name)
		nn := types.NamespacedName{Name: name, Namespace: s.TargetNamespace}

		ociRepo := &sourcev1.OCIRepository{}
		if err := s.Client.Get(ctx, nn, ociRepo); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}

			return fmt.Errorf("getting internal oci repository %s: %w", name, err)
		}

		base := ociRepo.DeepCopy()
		setReconcileRequestAnnotations(ociRepo)

		// The internal repository may be removed between the get and the patch,
		// which is the same case as the one skipped above.
		if err := s.Client.Patch(ctx, ociRepo, client.MergeFrom(base)); client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("requesting reconciliation of internal oci repository %s: %w", name, err)
		}
	}

	return nil
}

func (s *OCIRepoService) CleanupOCIRepository(ctx context.Context, repoName string) error {
	resources := []struct {
		name string
		obj  client.Object
	}{
		{
			name: utils.GetInternalRepositoryAuthSecretName(repoName),
			obj:  &corev1.Secret{},
		},
		{
			name: utils.GetInternalRepositoryTLSSecretName(repoName),
			obj:  &corev1.Secret{},
		},
	}

	for _, r := range resources {
		nn := types.NamespacedName{Name: r.name, Namespace: s.TargetNamespace}
		if err := s.ensureResourceDeleted(ctx, nn, r.obj); err != nil {
			return fmt.Errorf("cleaning up %T %s: %w", r.obj, r.name, err)
		}
	}

	return nil
}

// RemoveOCIRepository issues a delete for the internal OCIRepository and returns
// it while it is still present, so the caller can inspect its conditions and wait
// for nelm-source-controller to finish removing it. It returns nil once the
// OCIRepository is gone.
func (s *OCIRepoService) RemoveOCIRepository(ctx context.Context, addon *helmv1alpha1.HelmClusterAddon) (*sourcev1.OCIRepository, error) {
	name := utils.GetInternalOCIRepositoryName(addon.Name)
	nn := types.NamespacedName{Name: name, Namespace: s.TargetNamespace}
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
	addon *helmv1alpha1.HelmClusterAddon,
	repo *helmv1alpha1.HelmClusterAddonRepository,
	mediaType string,
	existing *sourcev1.OCIRepository,
) {
	if addon.ForceReconcileRequired() {
		setReconcileRequestAnnotations(existing)
	}

	existing.Spec.URL = repo.Spec.URL
	existing.Spec.Reference = &sourcev1.OCIRepositoryRef{
		Tag: addon.Spec.Chart.Version,
	}
	existing.Spec.Interval = metav1.Duration{Duration: InternalRepositoryInterval}
	existing.Spec.Insecure = repo.Spec.InsecureSkipVerify
	existing.Spec.CertSecretRef = nil
	existing.Spec.SecretRef = nil

	if repo.Spec.Auth != nil {
		existing.Spec.SecretRef = &meta.LocalObjectReference{
			Name: utils.GetInternalRepositoryAuthSecretName(repo.Name),
		}
	}

	if repo.Spec.CACertificate != "" {
		existing.Spec.CertSecretRef = &meta.LocalObjectReference{
			Name: utils.GetInternalRepositoryTLSSecretName(repo.Name),
		}
	}

	// The media type is the one recorded for this chart version by the repository
	// synchronization: it differs between charts pushed by current and by older tooling.
	// The caller guarantees it is non-empty.
	existing.Spec.LayerSelector = &sourcev1.OCILayerSelector{
		MediaType: mediaType,
		Operation: "copy",
	}

	existing.Labels = map[string]string{
		helmv1alpha1.LabelManagedBy:                  helmv1alpha1.LabelManagedByValue,
		helmv1alpha1.HelmClusterAddonLabelSourceName: addon.Name,
	}
}
