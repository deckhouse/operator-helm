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
	"maps"
	"strings"
	"time"

	helmv2 "github.com/fluxcd/helm-controller/api/v2"
	sourcev1 "github.com/fluxcd/source-controller/api/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
	"github.com/deckhouse/operator-helm/internal/chartsource"
	"github.com/deckhouse/operator-helm/internal/source"
)

const releaseDriftDetectionInterval = 5 * time.Minute

var helmReleaseErrorRules = []ErrorConditionRule{
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

func (s *ReleaseService) EnsureHelmRelease(ctx context.Context, rel source.Release, sourceKind chartsource.Kind, artifactRevision string) ReleaseOutcome {
	logger := log.FromContext(ctx)

	existing := &helmv2.HelmRelease{
		ObjectMeta: metav1.ObjectMeta{
			Name:      rel.InternalNames().HelmRelease,
			Namespace: s.TargetNamespace,
		},
	}

	op, err := controllerutil.CreateOrPatch(ctx, s.Client, existing, func() error {
		return applyHelmReleaseSpec(rel, existing, sourceKind, s.TargetNamespace)
	})
	if err != nil {
		return ReleaseOutcome{Err: fmt.Errorf("reconciling helm release: %w", err)}
	}

	internal := reduceInternalConditions(existing.GetConditions(), existing.Generation, helmReleaseErrorRules)

	// Whether the deployed revision is the one the spec asks for is a fact about the
	// object, so it is reported rather than acted on here: a chart-version change
	// updates the referenced HelmChart artifact without touching the HelmRelease spec
	// or generation, so the object can go on reporting the previous revision ready.
	deployed := isDesiredChartDeployed(rel, existing.Status.History.Latest(), artifactRevision)

	if internal.Ready() && deployed {
		logger.Info("Successfully reconciled helm release", "operation", op,
			"internalObject", client.ObjectKeyFromObject(existing))
	}

	return ReleaseOutcome{
		History:       existing.Status.History,
		Internal:      internal,
		ChartDeployed: deployed,
	}
}

// CleanupHelmRelease issues a delete for the internal HelmRelease and returns it
// while it is still present, so the caller can inspect its conditions and wait
// for helm-controller to finish uninstalling before proceeding. It returns nil
// once the HelmRelease is gone.
func (s *ReleaseService) CleanupHelmRelease(ctx context.Context, names source.ReleaseNames) (*helmv2.HelmRelease, error) {
	nn := types.NamespacedName{Name: names.HelmRelease, Namespace: s.TargetNamespace}
	release := &helmv2.HelmRelease{}
	exists, err := s.deleteAndCheck(ctx, nn, release)
	if err != nil {
		return nil, fmt.Errorf("failed to delete helm release: %w", err)
	}
	if !exists {
		return nil, nil
	}

	return release, nil
}

// SyncReleaseSpec re-applies the addon-derived fields onto an existing HelmRelease
// while the addon is being deleted, without ever creating it, and always requests
// a reconcile. A spec error propagated into the HelmRelease can make helm
// uninstall fail, and helm-controller re-reads the spec on every deletion
// reconcile, so re-applying the (possibly corrected) addon spec lets it retry
// with the fix. The reconcile request is stamped unconditionally: the cause of a
// failed uninstall may be external (e.g. kube-apiserver issues) and leave the
// spec unchanged, so helm-controller must be nudged out of its error backoff to
// retry on every pass regardless.
func (s *ReleaseService) SyncReleaseSpec(ctx context.Context, rel source.Release, release *helmv2.HelmRelease) error {
	base := release.DeepCopy()

	release.Spec.TargetNamespace = rel.TargetNamespace()
	release.Spec.Values = rel.Values()
	release.Spec.Timeout = rel.Timeout()
	release.Spec.Suspend = rel.MaintenanceActivated()

	setReconcileRequestAnnotations(release)

	// The release may finish deleting between the caller's get and this patch;
	// a NotFound then simply means the uninstall completed, so there is nothing
	// left to sync.
	if err := s.Client.Patch(ctx, release, client.MergeFrom(base)); client.IgnoreNotFound(err) != nil {
		return fmt.Errorf("syncing helm release spec during deletion: %w", err)
	}

	return nil
}

func applyHelmReleaseSpec(rel source.Release, existing *helmv2.HelmRelease, sourceKind chartsource.Kind, targetNamespace string) error {
	if rel.ForceReconcileRequired() {
		setReconcileRequestAnnotations(existing)
	}

	// Merge rather than replace: the internal HelmRelease may carry labels put
	// there by someone else (a policy engine, a cost allocator), and dropping them
	// on every pass would fight whoever set them.
	if existing.Labels == nil {
		existing.Labels = map[string]string{}
	}
	maps.Copy(existing.Labels, rel.SourceLabels())

	names := rel.InternalNames()

	existing.Spec.ReleaseName = rel.ReleaseName()
	existing.Spec.TargetNamespace = rel.TargetNamespace()
	existing.Spec.Values = rel.Values()
	existing.Spec.Timeout = rel.Timeout()

	existing.Spec.Suspend = rel.MaintenanceActivated()

	existing.Spec.Interval = metav1.Duration{Duration: releaseDriftDetectionInterval}

	existing.Spec.DriftDetection = &helmv2.DriftDetection{
		Mode: helmv2.DriftDetectionEnabled,
	}

	// A family that impersonates applies the chart as its own ServiceAccount, which
	// lives in the operator namespace next to the HelmRelease; helm-controller then
	// performs every cluster operation — the storage writes included — as that
	// account, so the release storage has to sit where the account has rights: the
	// target namespace. A family without a service account keeps helm-controller's
	// own identity and the default storage location.
	if names.ServiceAccount != "" {
		existing.Spec.ServiceAccountName = names.ServiceAccount
		existing.Spec.StorageNamespace = rel.TargetNamespace()
	}

	switch sourceKind {
	case chartsource.Helm:
		existing.Spec.ChartRef = &helmv2.CrossNamespaceSourceReference{
			Kind:      sourcev1.HelmChartKind,
			Name:      names.HelmChart,
			Namespace: targetNamespace,
		}
	case chartsource.OCI:
		existing.Spec.ChartRef = &helmv2.CrossNamespaceSourceReference{
			Kind:      sourcev1.OCIRepositoryKind,
			Name:      names.OCIRepository,
			Namespace: targetNamespace,
		}
	default:
		return fmt.Errorf("invalid chart source: %s", sourceKind)
	}

	return nil
}

// isDesiredChartDeployed reports whether the latest release revision in history
// is actually deployed and corresponds to the chart requested by the release spec.
func isDesiredChartDeployed(rel source.Release, latest *helmv2.Snapshot, artifactRevision string) bool {
	if latest == nil || latest.Status != "deployed" {
		return false
	}

	desired := rel.ChartRef().Version

	if latest.OCIDigest != "" {
		// The history says the deployed chart came from a registry, but the revision
		// the source now offers need not: a version the index republishes as an
		// archive is resolved through a HelmChart, whose revision is a bare version
		// with no digest to compare against. That is a chart other than the deployed
		// one, not a reason to index into a revision that has no digest part.
		_, latestDigest, ok := strings.Cut(artifactRevision, "@")
		if !ok || len(latestDigest) < 19 {
			return false
		}

		desiredVersion := desired + "+" + latestDigest[7:19]

		return latest.OCIDigest == latestDigest && latest.ChartVersion == desiredVersion
	}

	if desired != "" {
		return latest.ChartVersion == desired
	}

	return false
}
