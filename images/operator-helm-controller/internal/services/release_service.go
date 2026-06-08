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
	"strings"
	"time"

	"github.com/werf/3p-fluxcd-pkg/apis/meta"
	helmv2 "github.com/werf/3p-helm-controller/api/v2"
	sourcev1 "github.com/werf/nelm-source-controller/api/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
	"github.com/deckhouse/operator-helm/internal/manager/status"
	"github.com/deckhouse/operator-helm/internal/utils"
)

var helmReleaseErrorRules = []status.ErrorConditionRule{
	{Type: "Released", TriggerStatus: metav1.ConditionFalse, Reason: helmv1alpha1.ReasonReleaseFailed},
	{Type: "TestSuccess", TriggerStatus: metav1.ConditionFalse, Reason: helmv1alpha1.ReasonTestFailed},
	{Type: "Remediated", TriggerStatus: metav1.ConditionTrue, Reason: helmv1alpha1.ReasonRemediated},
}

type ReleaseService struct {
	BaseService

	TargetNamespace string
}

func NewReleaseService(client client.Client, scheme *runtime.Scheme, targetNamespace string) *ReleaseService {
	return &ReleaseService{
		BaseService: BaseService{
			Client: client,
			Scheme: scheme,
		},
		TargetNamespace: targetNamespace,
	}
}

var _ status.Provider = (*ReleaseResult)(nil)

type ReleaseResult struct {
	Status  status.Status
	History helmv2.Snapshots
}

func (r ReleaseResult) GetStatus() status.Status {
	return r.Status
}

func (r ReleaseResult) IsReady() bool {
	return r.Status.IsReady()
}

func (r ReleaseResult) GetConditionType() string {
	return helmv1alpha1.ConditionTypeReady
}

func (s *ReleaseService) EnsureHelmRelease(ctx context.Context, addon *helmv1alpha1.HelmClusterAddon, repoType utils.InternalRepositoryType, artifactRevision string) ReleaseResult {
	logger := log.FromContext(ctx)

	existing := &helmv2.HelmRelease{
		ObjectMeta: metav1.ObjectMeta{
			Name:      utils.GetInternalHelmReleaseName(addon.Name),
			Namespace: s.TargetNamespace,
		},
	}

	op, err := controllerutil.CreateOrPatch(ctx, s.Client, existing, func() error {
		return applyHelmReleaseSpec(addon, existing, repoType, s.TargetNamespace)
	})
	if err != nil {
		return ReleaseResult{Status: status.Failed(
			addon,
			helmv1alpha1.ReasonReleaseFailed,
			"Failed to create helm release",
			fmt.Errorf("reconciling helm release: %w", err),
		)}
	}

	processedStatus := status.ProcessChildConditions(
		existing.GetConditions(), existing.Generation, addon, helmReleaseErrorRules,
	)

	// A chart-version change updates only the referenced HelmChart artifact, not
	// the HelmRelease spec/generation. The HelmRelease can therefore still report
	// the readiness of the previous revision until it observes the new artifact.
	// Downgrade the status to Reconciling until the deployed revision actually
	// reflects the requested chart, so downstream consumers (lastAppliedChart and
	// the projected Ready/UpdateInstalled conditions) do not advance prematurely.
	if processedStatus.IsReady() && !isDesiredChartDeployed(addon, existing.Status.History.Latest(), artifactRevision) {
		processedStatus = status.Unknown(addon, helmv1alpha1.ReasonReconciling)
	}

	if processedStatus.IsReady() {
		logger.Info("Successfully reconciled helm release", "operation", op)
	}

	return ReleaseResult{
		History: existing.Status.History,
		Status:  processedStatus,
	}
}

func (s *ReleaseService) CleanupHelmRelease(ctx context.Context, addon *helmv1alpha1.HelmClusterAddon) error {
	nn := types.NamespacedName{Name: utils.GetInternalHelmReleaseName(addon.Name), Namespace: s.TargetNamespace}
	if err := s.ensureResourceDeleted(ctx, nn, &helmv2.HelmRelease{}); err != nil {
		return fmt.Errorf("failed to delete helm release: %w", err)
	}

	return nil
}

func applyHelmReleaseSpec(addon *helmv1alpha1.HelmClusterAddon, existing *helmv2.HelmRelease, repoType utils.InternalRepositoryType, targetNamespace string) error {
	if addon.ForceReconcileRequired() {
		if existing.Annotations == nil {
			existing.Annotations = map[string]string{}
		}
		ts := time.Now().UTC().Format(time.RFC3339)
		existing.Annotations[meta.ForceRequestAnnotation] = ts
		existing.Annotations[meta.ReconcileRequestAnnotation] = ts
	}

	if existing.Labels == nil {
		existing.Labels = map[string]string{}
	}

	existing.Labels[helmv1alpha1.LabelManagedBy] = helmv1alpha1.LabelManagedByValue
	existing.Labels[helmv1alpha1.HelmClusterAddonLabelSourceName] = addon.Name

	existing.Spec.ReleaseName = addon.Name
	existing.Spec.TargetNamespace = addon.Spec.Namespace
	existing.Spec.Values = addon.Spec.Values

	existing.Spec.Suspend = false

	if addon.Spec.Maintenance == string(helmv1alpha1.NoResourceReconciliation) {
		existing.Spec.Suspend = true
	}

	switch repoType {
	case utils.InternalHelmRepository:
		existing.Spec.ChartRef = &helmv2.CrossNamespaceSourceReference{
			Kind:      sourcev1.HelmChartKind,
			Name:      utils.GetInternalHelmChartName(addon.Name),
			Namespace: targetNamespace,
		}
	case utils.InternalOCIRepository:
		existing.Spec.ChartRef = &helmv2.CrossNamespaceSourceReference{
			Kind:      sourcev1.OCIRepositoryKind,
			Name:      utils.GetInternalOCIRepositoryName(addon.Name),
			Namespace: targetNamespace,
		}
	default:
		return fmt.Errorf("invalid repository type: %s", repoType)
	}

	return nil
}

// isDesiredChartDeployed reports whether the latest release revision in history
// is actually deployed and corresponds to the chart requested by the addon spec.
func isDesiredChartDeployed(addon *helmv1alpha1.HelmClusterAddon, latest *helmv2.Snapshot, artifactRevision string) bool {
	if latest == nil || latest.Status != "deployed" {
		return false
	}

	if latest.OCIDigest != "" {
		ociDigestParts := strings.Split(artifactRevision, "@")
		latestDigest := ociDigestParts[1]
		desiredVersion := addon.Spec.Chart.Version + "+" + latestDigest[7:19]

		return latest.OCIDigest == latestDigest && latest.ChartVersion == desiredVersion
	}

	if addon.Spec.Chart.Version != "" {
		return latest.ChartVersion == addon.Spec.Chart.Version
	}

	return false
}
