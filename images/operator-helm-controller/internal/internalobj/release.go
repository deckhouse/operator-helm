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
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	helmv2 "github.com/fluxcd/helm-controller/api/v2"
	sourcev1 "github.com/fluxcd/source-controller/api/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
	"github.com/deckhouse/operator-helm/internal/chartsource"
	"github.com/deckhouse/operator-helm/internal/contract"
	"github.com/deckhouse/operator-helm/internal/labels"
	"github.com/deckhouse/operator-helm/internal/utils"
)

const releaseDriftDetectionInterval = 5 * time.Minute

var helmReleaseErrorRules = []ErrorConditionRule{
	{Type: "Released", TriggerStatus: metav1.ConditionFalse, Reason: helmv1alpha1.ReasonReleaseFailed},
	{Type: "TestSuccess", TriggerStatus: metav1.ConditionFalse, Reason: helmv1alpha1.ReasonTestFailed},
	{Type: "Remediated", TriggerStatus: metav1.ConditionTrue, Reason: helmv1alpha1.ReasonRemediated},
}

type Releases struct {
	client          client.Client
	targetNamespace string
}

func NewReleases(client client.Client, targetNamespace string) *Releases {
	return &Releases{client: client, targetNamespace: targetNamespace}
}

func (s *Releases) Ensure(
	ctx context.Context,
	rel contract.Release,
	sourceKind chartsource.Kind,
	artifactRevision string,
	artifactCurrent bool,
) ReleaseOutcome {
	logger := log.FromContext(ctx)

	existing := &helmv2.HelmRelease{
		ObjectMeta: metav1.ObjectMeta{
			Name:      rel.InternalNames().HelmRelease,
			Namespace: s.targetNamespace,
		},
	}

	op, err := controllerutil.CreateOrPatch(ctx, s.client, existing, func() error {
		return applyHelmReleaseSpec(rel, existing, sourceKind, s.targetNamespace)
	})
	if err != nil {
		return ReleaseOutcome{Err: fmt.Errorf("reconciling helm release: %w", err)}
	}

	internal := reduceInternalConditions(existing.GetConditions(), existing.Generation, helmReleaseErrorRules)

	// A chart-version change moves the artifact without touching the HelmRelease spec or generation.
	deployed := artifactCurrent && isDesiredChartDeployed(rel, existing.Status.History.Latest(), artifactRevision)

	if internal.Ready() && deployed {
		logger.Info("Successfully reconciled helm release", "operation", op,
			"internalObject", client.ObjectKeyFromObject(existing))
	}

	return ReleaseOutcome{
		History:       existing.Status.History,
		Internal:      internal,
		ChartDeployed: deployed,
		Inventory:     parseInventory(ctx, existing.Status.Inventory),
	}
}

func parseInventory(ctx context.Context, inventory *helmv2.ResourceInventory) []helmv1alpha1.InventoryEntry {
	if inventory == nil {
		return nil
	}

	entries := make([]helmv1alpha1.InventoryEntry, 0, len(inventory.Entries))

	for _, ref := range inventory.Entries {
		entry, err := parseInventoryID(ref.ID, ref.Version)
		if err != nil {
			log.FromContext(ctx).Error(err, "Skipping an unparsable inventory entry", "id", ref.ID)

			continue
		}

		entries = append(entries, entry)
	}

	if len(entries) == 0 {
		return nil
	}

	// The status patch compares the whole field, so an unstable order would patch every pass.
	slices.SortFunc(entries, func(a, b helmv1alpha1.InventoryEntry) int {
		return cmp.Or(
			strings.Compare(a.Kind, b.Kind),
			strings.Compare(a.APIVersion, b.APIVersion),
			strings.Compare(a.Namespace, b.Namespace),
			strings.Compare(a.Name, b.Name),
		)
	})

	return entries
}

func parseInventoryID(id, version string) (helmv1alpha1.InventoryEntry, error) {
	malformed := fmt.Errorf("unable to parse stored object metadata: %s", id)

	namespace, rest, ok := strings.Cut(id, "_")
	if !ok {
		return helmv1alpha1.InventoryEntry{}, malformed
	}

	rest, kind, ok := cutLast(rest, "_")
	if !ok {
		return helmv1alpha1.InventoryEntry{}, malformed
	}

	name, group, ok := cutLast(rest, "_")
	if !ok {
		return helmv1alpha1.InventoryEntry{}, malformed
	}

	// Mirrors ParseObjMetadata of fluxcd/cli-utils, which builds the id: an RBAC name carries
	// its colons transcoded and an underscore of its own, so the name is never split on.
	name = strings.ReplaceAll(name, "__", ":")

	apiVersion := version
	if group != "" {
		apiVersion = group + "/" + version
	}

	return helmv1alpha1.InventoryEntry{
		APIVersion: apiVersion,
		Kind:       kind,
		Namespace:  namespace,
		Name:       name,
	}, nil
}

func cutLast(s, sep string) (before, after string, found bool) {
	i := strings.LastIndex(s, sep)
	if i < 0 {
		return s, "", false
	}

	return s[:i], s[i+len(sep):], true
}

// Inventory is what the internal HelmRelease currently records as deployed. It serves the
// passes that do not get as far as writing the release: without it a release that settled
// before the field existed would wait for a spec change to publish a list already there.
func (s *Releases) Inventory(ctx context.Context, names contract.ReleaseNames) []helmv1alpha1.InventoryEntry {
	release := &helmv2.HelmRelease{}
	key := types.NamespacedName{Name: names.HelmRelease, Namespace: s.targetNamespace}

	if err := s.client.Get(ctx, key, release); err != nil {
		if !apierrors.IsNotFound(err) {
			log.FromContext(ctx).Error(err, "Failed to read the inventory of the internal release",
				"internalObject", key)
		}

		return nil
	}

	return parseInventory(ctx, release.Status.Inventory)
}

// Cleanup issues a delete for the internal HelmRelease and returns it while it is still
// present, so the caller can wait for helm-controller to finish uninstalling it.
func (s *Releases) Cleanup(ctx context.Context, names contract.ReleaseNames) (*helmv2.HelmRelease, error) {
	nn := types.NamespacedName{Name: names.HelmRelease, Namespace: s.targetNamespace}
	release := &helmv2.HelmRelease{}
	exists, err := utils.DeleteIfPresent(ctx, s.client, nn, release)
	if err != nil {
		return nil, fmt.Errorf("failed to delete helm release: %w", err)
	}
	if !exists {
		return nil, nil
	}

	return release, nil
}

// SyncSpec re-applies the addon-derived fields onto an existing HelmRelease while the addon
// is being deleted, without ever creating it, and always requests a reconcile. A suspended
// release is resumed: helm-controller drops a suspended one without uninstalling it.
func (s *Releases) SyncSpec(ctx context.Context, rel contract.Release, release *helmv2.HelmRelease) error {
	base := release.DeepCopy()

	release.Spec.TargetNamespace = rel.TargetNamespace()
	release.Spec.Values = rel.Values()
	release.Spec.Timeout = rel.Timeout()
	release.Spec.Suspend = false

	setReconcileRequestAnnotations(release)

	// A NotFound means the uninstall completed between the caller's get and this patch.
	if err := s.client.Patch(ctx, release, client.MergeFrom(base)); client.IgnoreNotFound(err) != nil {
		return fmt.Errorf("syncing helm release spec during deletion: %w", err)
	}

	return nil
}

func applyHelmReleaseSpec(rel contract.Release, existing *helmv2.HelmRelease, sourceKind chartsource.Kind, targetNamespace string) error {
	if rel.ForceReconcileRequired() {
		setReconcileRequestAnnotations(existing)
	}

	labels.ApplyOwned(existing, rel.SourceLabels())

	names := rel.InternalNames()

	existing.Spec.ReleaseName = rel.ReleaseName()
	existing.Spec.TargetNamespace = rel.TargetNamespace()
	existing.Spec.Values = rel.Values()
	existing.Spec.Timeout = rel.Timeout()
	existing.Spec.CommonMetadata = &helmv2.CommonMetadata{Labels: rel.SourceLabels()}

	existing.Spec.Suspend = rel.MaintenanceActivated()

	existing.Spec.Interval = metav1.Duration{Duration: releaseDriftDetectionInterval}

	existing.Spec.DriftDetection = &helmv2.DriftDetection{
		Mode: helmv2.DriftDetectionEnabled,
	}

	// helm-controller writes the release storage as this account, so it must sit where the account has rights.
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

func isDesiredChartDeployed(rel contract.Release, latest *helmv2.Snapshot, artifactRevision string) bool {
	if latest == nil || latest.Status != "deployed" {
		return false
	}

	if latest.OCIDigest != "" {
		// A revision carrying no digest is a chart other than the deployed one, not one to index into.
		_, latestDigest, ok := strings.Cut(artifactRevision, "@")
		if !ok {
			return false
		}

		// The digest, not the chart version: a registry may publish a chart under a differently spelled tag.
		return recordedOCIDigest(latest.OCIDigest) == latestDigest
	}

	if desired := rel.ChartRef().Version; desired != "" {
		return latest.ChartVersion == desired
	}

	return false
}

func recordedOCIDigest(ociDigest string) string {
	// helm-controller records the digest alone for a semver tag and `<tag>@<algorithm>:<digest>` otherwise.
	if _, digest, ok := strings.Cut(ociDigest, "@"); ok {
		return digest
	}

	return ociDigest
}
