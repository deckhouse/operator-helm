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

// Claims enforces that at most one HelmClusterAddon uses a given
// repository/chart pair. A validating webhook cannot guarantee this: admission
// requests are served concurrently and before the object is persisted, so two
// simultaneous creates can both pass. Uniqueness is instead delegated to the API
// server's only atomic cross-object primitive — the uniqueness of an object name.
// Each repository/chart pair maps to a single Lease name; whoever creates that
// Lease first owns the pair, and every other addon reconciles into a conflict.
//
// It is addon-only in substance, not just by convention: the Lease name carries
// neither the release kind nor a namespace, and the holder recorded on it is read
// back as a HelmClusterAddon. Another family needing uniqueness would have to
// bring its own implementation of the reconciler's Claims rather than reuse this one.
type Claims struct {
	// reader reads from the API server directly (mgr.GetAPIReader()), bypassing the
	// controller cache: an acquisition decision must never be made against stale data.
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

// Acquire ensures the addon owns the claim for its repository/chart pair. It
// reports whether the claim is held by this addon; when it is not, holder names
// the addon that currently owns it so the caller can surface a conflict.
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
		// Lost the create race against a concurrent reconcile; re-read the winning
		// Lease authoritatively and fall through to the ownership check.
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

	// The recorded holder is gone; take the orphaned Lease over.
	// The update is optimistic: if another addon takes it over first, our write is
	// rejected with a conflict and we report the pair as claimed and requeue.
	if err := s.takeOver(ctx, lease, rel); err != nil {
		if apierrors.IsConflict(err) {
			return false, holder, nil
		}
		return false, "", err
	}

	return true, rel.Name(), nil
}

// OwnedBy reports whether the addon currently holds the claim Lease for its
// repository/chart pair. It reads through the direct reader for the same reason
// Acquire does: an ownership decision must not be made against stale cache data.
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

// Release deletes the claim Lease, but only if this addon still owns it, so a
// duplicate addon that never acquired the pair cannot free the real owner's claim.
func (s *Claims) Drop(ctx context.Context, rel contract.Release) error {
	nn := s.leaseKey(rel)

	lease := &coordinationv1.Lease{}
	if err := s.reader.Get(ctx, nn, lease); err != nil {
		return client.IgnoreNotFound(err)
	}

	if leaseHolder(lease) != rel.Name() {
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

// DropStale deletes claim Leases this addon still owns for repository/chart
// pairs it no longer references — e.g. after the chart ref changed. The Lease name
// is derived from the current spec, so a chart change makes Acquire claim a new
// Lease and orphan the old one; the previous pair is not stored anywhere. The
// orphans are found instead by the source-name label every claim already carries.
// Only the addon's own claims (by HolderIdentity) other than the current one are
// removed, so a pair taken over by another addon is left intact.
func (s *Claims) DropStale(ctx context.Context, rel contract.Release) error {
	// Freeing a pair is only safe once this release is known to have moved onto the
	// one it names now. Until then its chart may still be installed under this
	// release's name, and handing the pair to somebody else would have two releases
	// deploying one chart into one namespace — the collision the claim exists to
	// prevent.
	//
	// The status is what says the move happened, and it says it late: it is written
	// after the chart is applied, so a release that has just installed something may
	// still carry no record of it at all. An empty record therefore cannot be read as
	// "nothing is deployed" — it is read as "unknown", and nothing is released.
	//
	// This keeps a pair the release cannot move off — a spec pointed at a chart
	// somebody else holds never reaches the status — until the release is deleted,
	// when teardown frees everything it holds. That is the cheaper failure: a pair
	// held too long needs someone to look at it, a pair freed too early corrupts an
	// installation.
	last := rel.LastAppliedChart()
	if last == nil {
		return nil
	}

	current := s.leaseKey(rel).Name
	if utils.GetChartClaimLeaseName(last.Repository.Name, last.Chart) != current {
		return nil
	}

	return s.dropOwned(ctx, rel, current)
}

// DropAll releases every claim this release holds, the pair it names included. It
// is the teardown counterpart of DropStale: nothing of this release is deployed any
// more by the time it runs, so there is no pair left to protect, and no later pass
// of anybody's would be allowed to free what it left behind.
func (s *Claims) DropAll(ctx context.Context, rel contract.Release) error {
	return s.dropOwned(ctx, rel, "", "")
}

// dropOwned deletes the claim Leases this release holds, except those named by keep.
func (s *Claims) dropOwned(ctx context.Context, rel contract.Release, keep ...string) error {
	kept := func(name string) bool {
		for _, k := range keep {
			if k != "" && k == name {
				return true
			}
		}

		return false
	}

	// Direct reader, not the cached client: claim Leases are not watched, so the
	// informer cache has no data for them (same reason Acquire/Release use reader).
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

// isHolderStale reports whether the recorded holder no longer keeps this claim,
// which it does only by ceasing to exist. A Lease left behind by a deletion is an
// orphan nobody else would ever clear, so it is taken over here.
//
// A holder that is still there keeps its claim, whatever its spec now says. Giving
// it up is the holder's own act — DropStale removes the Leases it holds for pairs it
// no longer names, and Drop removes them all when it is deleted — and reading its
// spec here instead would hand the pair out twice. The holder does not write to the
// Lease when it finds itself already recorded in it, so an addon that moves off this
// chart and back leaves the Lease untouched, and the optimistic update below is
// happy to overwrite it: two addons would each believe they had the claim, and one
// of them would have been told so after the other had resumed using it.
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
