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

	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
	"github.com/deckhouse/operator-helm/internal/utils"
)

// ClaimService enforces that at most one HelmClusterAddon uses a given
// repository/chart pair. A validating webhook cannot guarantee this: admission
// requests are served concurrently and before the object is persisted, so two
// simultaneous creates can both pass. Uniqueness is instead delegated to the API
// server's only atomic cross-object primitive — the uniqueness of an object name.
// Each repository/chart pair maps to a single Lease name; whoever creates that
// Lease first owns the pair, and every other addon reconciles into a conflict.
type ClaimService struct {
	// reader reads from the API server directly (mgr.GetAPIReader()), bypassing the
	// controller cache: an acquisition decision must never be made against stale data.
	reader    client.Reader
	client    client.Client
	namespace string
}

func NewClaimService(c client.Client, reader client.Reader, namespace string) *ClaimService {
	return &ClaimService{
		reader:    reader,
		client:    c,
		namespace: namespace,
	}
}

// Acquire ensures the addon owns the claim for its repository/chart pair. It
// reports whether the claim is held by this addon; when it is not, holder names
// the addon that currently owns it so the caller can surface a conflict.
func (s *ClaimService) Acquire(ctx context.Context, addon *helmv1alpha1.HelmClusterAddon) (acquired bool, holder string, err error) {
	nn := s.leaseKey(addon)

	lease := &coordinationv1.Lease{}
	getErr := s.reader.Get(ctx, nn, lease)
	switch {
	case apierrors.IsNotFound(getErr):
		created, createErr := s.create(ctx, nn, addon)
		if createErr != nil {
			return false, "", createErr
		}
		if created {
			return true, addon.Name, nil
		}
		// Lost the create race against a concurrent reconcile; re-read the winning
		// Lease authoritatively and fall through to the ownership check.
		if err := s.reader.Get(ctx, nn, lease); err != nil {
			return false, "", fmt.Errorf("getting chart claim lease: %w", err)
		}
	case getErr != nil:
		return false, "", fmt.Errorf("getting chart claim lease: %w", getErr)
	}

	holder = leaseHolder(lease)
	if holder == addon.Name {
		return true, holder, nil
	}

	stale, err := s.isHolderStale(ctx, holder, nn.Name)
	if err != nil {
		return false, "", err
	}
	if !stale {
		return false, holder, nil
	}

	// The recorded holder is gone or no longer uses this chart; take the Lease over.
	// The update is optimistic: if another addon takes it over first, our write is
	// rejected with a conflict and we report the pair as claimed and requeue.
	if err := s.takeOver(ctx, lease, addon); err != nil {
		if apierrors.IsConflict(err) {
			return false, holder, nil
		}
		return false, "", err
	}

	return true, addon.Name, nil
}

// Release deletes the claim Lease, but only if this addon still owns it, so a
// duplicate addon that never acquired the pair cannot free the real owner's claim.
func (s *ClaimService) Release(ctx context.Context, addon *helmv1alpha1.HelmClusterAddon) error {
	nn := s.leaseKey(addon)

	lease := &coordinationv1.Lease{}
	if err := s.reader.Get(ctx, nn, lease); err != nil {
		return client.IgnoreNotFound(err)
	}

	if leaseHolder(lease) != addon.Name {
		return nil
	}

	// Delete only this exact revision: if the Lease was taken over between the read
	// and the delete, the precondition fails and we leave the new owner's claim intact.
	err := s.client.Delete(ctx, lease, client.Preconditions{ResourceVersion: &lease.ResourceVersion})
	if apierrors.IsConflict(err) {
		return nil
	}
	if client.IgnoreNotFound(err) != nil {
		return fmt.Errorf("releasing chart claim lease: %w", err)
	}

	return nil
}

// ReleaseStale deletes claim Leases this addon still owns for repository/chart
// pairs it no longer references — e.g. after the chart ref changed. The Lease name
// is derived from the current spec, so a chart change makes Acquire claim a new
// Lease and orphan the old one; the previous pair is not stored anywhere. The
// orphans are found instead by the source-name label every claim already carries.
// Only the addon's own claims (by HolderIdentity) other than the current one are
// removed, so a pair taken over by another addon is left intact.
func (s *ClaimService) ReleaseStale(ctx context.Context, addon *helmv1alpha1.HelmClusterAddon) error {
	current := s.leaseKey(addon).Name

	// Direct reader, not the cached client: claim Leases are not watched, so the
	// informer cache has no data for them (same reason Acquire/Release use reader).
	leases := &coordinationv1.LeaseList{}
	if err := s.reader.List(ctx, leases,
		client.InNamespace(s.namespace),
		client.MatchingLabels{
			helmv1alpha1.LabelManagedBy:                  helmv1alpha1.LabelManagedByValue,
			helmv1alpha1.HelmClusterAddonLabelSourceName: addon.Name,
		},
	); err != nil {
		return fmt.Errorf("listing chart claim leases: %w", err)
	}

	for i := range leases.Items {
		lease := &leases.Items[i]
		if lease.Name == current || leaseHolder(lease) != addon.Name {
			continue
		}

		// Delete only this exact revision: if the Lease was taken over between the
		// list and the delete, the precondition fails and we leave it intact.
		err := s.client.Delete(ctx, lease, client.Preconditions{ResourceVersion: &lease.ResourceVersion})
		if apierrors.IsConflict(err) {
			continue
		}
		if client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("releasing stale chart claim lease %q: %w", lease.Name, err)
		}
	}

	return nil
}

func (s *ClaimService) create(ctx context.Context, nn types.NamespacedName, addon *helmv1alpha1.HelmClusterAddon) (created bool, err error) {
	holder := addon.Name
	lease := &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{
			Name:      nn.Name,
			Namespace: nn.Namespace,
			Labels:    claimLabels(addon),
		},
		Spec: coordinationv1.LeaseSpec{
			HolderIdentity: &holder,
		},
	}

	err = s.client.Create(ctx, lease)
	if apierrors.IsAlreadyExists(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("creating chart claim lease: %w", err)
	}

	return true, nil
}

func (s *ClaimService) takeOver(ctx context.Context, lease *coordinationv1.Lease, addon *helmv1alpha1.HelmClusterAddon) error {
	holder := addon.Name
	lease.Spec.HolderIdentity = &holder

	if lease.Labels == nil {
		lease.Labels = map[string]string{}
	}
	for k, v := range claimLabels(addon) {
		lease.Labels[k] = v
	}

	return s.client.Update(ctx, lease)
}

// isHolderStale reports whether the recorded holder no longer keeps this claim: it
// either no longer exists or its current spec points at a different chart, in which
// case the Lease is an orphan left behind by a deletion or a chart change.
func (s *ClaimService) isHolderStale(ctx context.Context, holder, leaseName string) (bool, error) {
	if holder == "" {
		return true, nil
	}

	other := &helmv1alpha1.HelmClusterAddon{}
	err := s.reader.Get(ctx, types.NamespacedName{Name: holder}, other)
	if apierrors.IsNotFound(err) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("getting chart claim holder: %w", err)
	}

	holderLease := utils.GetChartClaimLeaseName(
		other.Spec.Chart.HelmClusterAddonRepository, other.Spec.Chart.HelmClusterAddonChartName,
	)

	return holderLease != leaseName, nil
}

func (s *ClaimService) leaseKey(addon *helmv1alpha1.HelmClusterAddon) types.NamespacedName {
	return types.NamespacedName{
		Name:      utils.GetChartClaimLeaseName(addon.Spec.Chart.HelmClusterAddonRepository, addon.Spec.Chart.HelmClusterAddonChartName),
		Namespace: s.namespace,
	}
}

func leaseHolder(lease *coordinationv1.Lease) string {
	if lease.Spec.HolderIdentity == nil {
		return ""
	}

	return *lease.Spec.HolderIdentity
}

func claimLabels(addon *helmv1alpha1.HelmClusterAddon) map[string]string {
	return map[string]string{
		helmv1alpha1.LabelManagedBy:                  helmv1alpha1.LabelManagedByValue,
		helmv1alpha1.HelmClusterAddonLabelSourceName: addon.Name,
	}
}
