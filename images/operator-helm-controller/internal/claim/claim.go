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

package claim

import (
	"context"
	"fmt"

	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
	"github.com/deckhouse/operator-helm/internal/contract"
	"github.com/deckhouse/operator-helm/internal/utils"
)

// Claims enforces that at most one HelmClusterAddon uses a given repository/chart pair.
// Whoever creates the pair's Lease first owns it; every other addon reconciles into a conflict.
// It is valid for the cluster-addon kind alone: the holder is looked up as a HelmClusterAddon by
// name, and the Lease is keyed without the release's namespace.
type Claims struct {
	// Reads bypass the controller cache: an acquisition decision must not be made on stale data.
	reader    client.Reader
	client    client.Client
	namespace string
}

func NewClaims(c client.Client, reader client.Reader, namespace string) *Claims {
	return &Claims{
		reader:    reader,
		client:    c,
		namespace: namespace,
	}
}

// Acquire ensures the addon owns the claim for its repository/chart pair. When it does not,
// holder names the addon that currently owns it.
func (s *Claims) Acquire(ctx context.Context, rel contract.Release) (acquired bool, holder string, err error) {
	nn := s.leaseKey(rel)

	lease := &coordinationv1.Lease{}
	getErr := s.reader.Get(ctx, nn, lease)
	switch {
	case apierrors.IsNotFound(getErr):
		created, createErr := s.create(ctx, nn, rel)
		if createErr != nil {
			return false, "", createErr
		}
		if created {
			return true, rel.Name(), nil
		}
		// Lost the create race; re-read the winning Lease and fall through to the ownership check.
		if err := s.reader.Get(ctx, nn, lease); err != nil {
			return false, "", fmt.Errorf("getting chart claim lease: %w", err)
		}
	case getErr != nil:
		return false, "", fmt.Errorf("getting chart claim lease: %w", getErr)
	}

	holder = leaseHolder(lease)
	if holder == rel.Name() {
		return true, holder, nil
	}

	stale, err := s.isHolderStale(ctx, holder)
	if err != nil {
		return false, "", err
	}
	if !stale {
		return false, holder, nil
	}

	// The take-over is optimistic: another addon getting there first makes this a conflict.
	if err := s.takeOver(ctx, lease, rel); err != nil {
		if apierrors.IsConflict(err) {
			return false, holder, nil
		}
		return false, "", err
	}

	return true, rel.Name(), nil
}

// OwnedBy reports whether the addon currently holds the claim Lease for its repository/chart
// pair, read through the direct reader rather than the cache.
func (s *Claims) OwnedBy(ctx context.Context, rel contract.Release) (bool, error) {
	lease := &coordinationv1.Lease{}
	err := s.reader.Get(ctx, s.leaseKey(rel), lease)
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("getting chart claim lease: %w", err)
	}

	return leaseHolder(lease) == rel.Name(), nil
}

// Drop deletes the claim Lease, but only if this addon still owns it.
func (s *Claims) Drop(ctx context.Context, rel contract.Release) error {
	nn := s.leaseKey(rel)

	lease := &coordinationv1.Lease{}
	if err := s.reader.Get(ctx, nn, lease); err != nil {
		return client.IgnoreNotFound(err)
	}

	if leaseHolder(lease) != rel.Name() {
		return nil
	}

	// Delete only this exact revision, so a Lease taken over in the meantime is left intact.
	err := s.client.Delete(ctx, lease, client.Preconditions{ResourceVersion: &lease.ResourceVersion})
	if apierrors.IsConflict(err) {
		return nil
	}
	if client.IgnoreNotFound(err) != nil {
		return fmt.Errorf("releasing chart claim lease: %w", err)
	}

	return nil
}

// DropStale deletes the claim Leases this addon still owns for repository/chart pairs it no
// longer references and has recorded moving off, found by the source-name label on each claim.
func (s *Claims) DropStale(ctx context.Context, rel contract.Release) error {
	// An absent record reads as unknown, not as "nothing deployed", so nothing is freed.
	last := rel.LastAppliedChart()
	if last == nil {
		return nil
	}

	current := s.leaseKey(rel).Name
	if utils.GetChartClaimLeaseName(last.Repository.Name, last.Chart) != current {
		// Freed only once the record names the current pair: the one it still names may be deployed.
		return nil
	}

	return s.dropOwned(ctx, rel, current)
}

// DropAll releases every claim this release holds, the pair it names included. It is the
// teardown counterpart of DropStale.
func (s *Claims) DropAll(ctx context.Context, rel contract.Release) error {
	return s.dropOwned(ctx, rel)
}

// dropOwned deletes the claim Leases this release holds, except those named by keep.
func (s *Claims) dropOwned(ctx context.Context, rel contract.Release, keep ...string) error {
	kept := func(name string) bool {
		for _, k := range keep {
			if k == name {
				return true
			}
		}

		return false
	}

	// Direct reader, not the cached client: claim Leases are not watched, so the cache is empty.
	leases := &coordinationv1.LeaseList{}
	if err := s.reader.List(ctx, leases,
		client.InNamespace(s.namespace),
		client.MatchingLabels(rel.SourceLabels()),
	); err != nil {
		return fmt.Errorf("listing chart claim leases: %w", err)
	}

	for i := range leases.Items {
		lease := &leases.Items[i]
		if kept(lease.Name) || leaseHolder(lease) != rel.Name() {
			continue
		}

		// Delete only this exact revision, so a Lease taken over in the meantime is left intact.
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

func (s *Claims) create(ctx context.Context, nn types.NamespacedName, rel contract.Release) (created bool, err error) {
	holder := rel.Name()
	lease := &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{
			Name:      nn.Name,
			Namespace: nn.Namespace,
			Labels:    rel.SourceLabels(),
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

func (s *Claims) takeOver(ctx context.Context, lease *coordinationv1.Lease, rel contract.Release) error {
	holder := rel.Name()
	lease.Spec.HolderIdentity = &holder

	if lease.Labels == nil {
		lease.Labels = map[string]string{}
	}
	for k, v := range rel.SourceLabels() {
		lease.Labels[k] = v
	}

	return s.client.Update(ctx, lease)
}

// isHolderStale reports whether the recorded holder no longer keeps this claim, which it
// does only by ceasing to exist. A holder that still exists keeps it, whatever its spec says.
func (s *Claims) isHolderStale(ctx context.Context, holder string) (bool, error) {
	if holder == "" {
		return true, nil
	}

	err := s.reader.Get(ctx, types.NamespacedName{Name: holder}, &helmv1alpha1.HelmClusterAddon{})
	if apierrors.IsNotFound(err) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("getting chart claim holder: %w", err)
	}

	return false, nil
}

func (s *Claims) leaseKey(rel contract.Release) types.NamespacedName {
	ref := rel.ChartRef()
	return types.NamespacedName{
		Name:      utils.GetChartClaimLeaseName(ref.Repository.Name, ref.Chart),
		Namespace: s.namespace,
	}
}

func leaseHolder(lease *coordinationv1.Lease) string {
	if lease.Spec.HolderIdentity == nil {
		return ""
	}

	return *lease.Spec.HolderIdentity
}
