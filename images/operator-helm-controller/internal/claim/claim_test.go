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
	"errors"
	"testing"

	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
	"github.com/deckhouse/operator-helm/internal/adapter"
	"github.com/deckhouse/operator-helm/internal/utils"
)

// claimRepo is the repository every addon in this suite points at: the pair that
// carries the claim is the repository and the chart together, and the chart is what
// the cases vary.
const claimRepo = "stable"

func claimingAddon(name, chart string) *helmv1alpha1.HelmClusterAddon {
	return &helmv1alpha1.HelmClusterAddon{
		ObjectMeta: metav1.ObjectMeta{Name: name, Generation: 1},
		Spec: helmv1alpha1.HelmClusterAddonSpec{
			Namespace: "app",
			Chart: helmv1alpha1.HelmClusterAddonChartRef{
				HelmClusterAddonRepository: claimRepo,
				HelmClusterAddonChartName:  chart,
				Version:                    "6.7.1",
			},
		},
	}
}

// claimFor builds the Lease the package would find for the pair, held by holder.
// It derives the name through the same helper the package uses, so this suite pins
// what happens around the name rather than the derivation itself; the derivation is
// frozen by TestAddonInternalNamesAreFrozen, where the live object names are kept.
func claimFor(repo, chart, holder, labelSource string) *coordinationv1.Lease {
	return &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{
			Name:      utils.GetChartClaimLeaseName(repo, chart),
			Namespace: testNamespace,
			Labels: map[string]string{
				helmv1alpha1.LabelManagedBy:                  helmv1alpha1.LabelManagedByValue,
				helmv1alpha1.HelmClusterAddonLabelSourceName: labelSource,
			},
		},
		Spec: coordinationv1.LeaseSpec{HolderIdentity: &holder},
	}
}

func newClaims(t *testing.T, objects ...client.Object) (*Claims, client.Client) {
	t.Helper()

	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objects...).Build()

	return NewClaims(c, c, testNamespace), c
}

func claimHolder(t *testing.T, c client.Client, repo, chart string) string {
	t.Helper()

	lease := &coordinationv1.Lease{}
	key := client.ObjectKey{Namespace: testNamespace, Name: utils.GetChartClaimLeaseName(repo, chart)}
	if err := c.Get(context.Background(), key, lease); err != nil {
		t.Fatalf("getting claim lease: %v", err)
	}

	return leaseHolder(lease)
}

// TestAcquireClaimsAFreePair pins what a first acquisition writes: the Lease is
// named after the pair, lives in the operator namespace, records the addon as its
// holder and carries the source labels DropStale later finds orphans by.
func TestAcquireClaimsAFreePair(t *testing.T) {
	addon := claimingAddon("metrics", "podinfo")
	rel := adapter.NewAddonRelease(addon)
	claims, c := newClaims(t, addon)

	acquired, holder, err := claims.Acquire(context.Background(), rel)
	if err != nil {
		t.Fatalf("Acquire returned %v", err)
	}
	if !acquired || holder != "metrics" {
		t.Fatalf("Acquire = %v/%q, want the pair claimed by the addon", acquired, holder)
	}

	lease := &coordinationv1.Lease{}
	key := client.ObjectKey{Namespace: testNamespace, Name: utils.GetChartClaimLeaseName("stable", "podinfo")}
	if err := c.Get(context.Background(), key, lease); err != nil {
		t.Fatalf("the claim lease was not created: %v", err)
	}
	if got := leaseHolder(lease); got != "metrics" {
		t.Fatalf("holder = %q, want the acquiring addon", got)
	}
	for name, want := range rel.SourceLabels() {
		if lease.Labels[name] != want {
			t.Fatalf("label %s = %q, want %q: DropStale finds orphans by these", name, lease.Labels[name], want)
		}
	}
}

// TestAcquireIsIdempotentForTheHolder pins that the addon that already owns the
// pair keeps it: every pass calls Acquire, so a second call must not be read as a
// conflict with the addon itself.
func TestAcquireIsIdempotentForTheHolder(t *testing.T) {
	addon := claimingAddon("metrics", "podinfo")
	claims, _ := newClaims(t, addon, claimFor("stable", "podinfo", "metrics", "metrics"))

	acquired, holder, err := claims.Acquire(context.Background(), adapter.NewAddonRelease(addon))
	if err != nil {
		t.Fatalf("Acquire returned %v", err)
	}
	if !acquired || holder != "metrics" {
		t.Fatalf("Acquire = %v/%q, want the pair still claimed by its holder", acquired, holder)
	}
}

// TestAcquireReportsTheHolderOfATakenPair pins the case the whole type exists for:
// a second addon asking for a pair a live addon uses is refused and told who owns
// it, so the reconciler can name the conflict in the status.
func TestAcquireReportsTheHolderOfATakenPair(t *testing.T) {
	owner := claimingAddon("metrics", "podinfo")
	duplicate := claimingAddon("metrics-copy", "podinfo")
	claims, c := newClaims(t, owner, duplicate, claimFor("stable", "podinfo", "metrics", "metrics"))

	acquired, holder, err := claims.Acquire(context.Background(), adapter.NewAddonRelease(duplicate))
	if err != nil {
		t.Fatalf("Acquire returned %v", err)
	}
	if acquired {
		t.Fatal("a pair a live addon uses must not be handed to a second one")
	}
	if holder != "metrics" {
		t.Fatalf("holder = %q, want the addon that owns the pair", holder)
	}
	if got := claimHolder(t, c, "stable", "podinfo"); got != "metrics" {
		t.Fatalf("holder on the lease = %q, want it untouched", got)
	}
}

// TestAcquireTakesOverAClaimNoOneKeeps pins the one way a Lease outlives its
// holder — the addon is gone — and that an empty holder identity is as stale as a
// missing addon.
//
// An addon that merely moved to another chart is deliberately not among them: it is
// still there, and giving up a claim is its own act, done by DropStale on its next
// pass. See TestAcquireLeavesTheClaimOfALiveAddonThatMovedAway.
func TestAcquireTakesOverAClaimNoOneKeeps(t *testing.T) {
	tests := []struct {
		name    string
		objects []client.Object
	}{
		{
			name:    "the holder was deleted",
			objects: []client.Object{claimFor("stable", "podinfo", "gone", "gone")},
		},
		{
			name:    "the lease records no holder at all",
			objects: []client.Object{claimFor("stable", "podinfo", "", "")},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			addon := claimingAddon("metrics", "podinfo")
			claims, c := newClaims(t, append(test.objects, addon)...)

			acquired, holder, err := claims.Acquire(context.Background(), adapter.NewAddonRelease(addon))
			if err != nil {
				t.Fatalf("Acquire returned %v", err)
			}
			if !acquired || holder != "metrics" {
				t.Fatalf("Acquire = %v/%q, want the orphaned claim taken over", acquired, holder)
			}
			if got := claimHolder(t, c, "stable", "podinfo"); got != "metrics" {
				t.Fatalf("holder on the lease = %q, want the addon that took it over", got)
			}
		})
	}
}

// TestAcquireRereadsTheLeaseAfterLosingTheCreateRace pins the race the Lease exists
// to settle: two addons see the pair as free, one create wins and the loser must
// learn the winner's name from the API server rather than report an error.
func TestAcquireRereadsTheLeaseAfterLosingTheCreateRace(t *testing.T) {
	addon := claimingAddon("metrics-copy", "podinfo")
	winner := claimingAddon("metrics", "podinfo")

	var refused bool
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(addon, winner).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				lease, ok := obj.(*coordinationv1.Lease)
				if !ok || refused {
					return cl.Create(ctx, obj, opts...)
				}

				// The winner stores its claim first; this pass learns of it the way
				// the API server would report it.
				refused = true
				if err := cl.Create(ctx, claimFor("stable", "podinfo", "metrics", "metrics"), opts...); err != nil {
					return err
				}

				return apierrors.NewAlreadyExists(coordinationv1.Resource("leases"), lease.Name)
			},
		}).
		Build()
	claims := NewClaims(c, c, testNamespace)

	acquired, holder, err := claims.Acquire(context.Background(), adapter.NewAddonRelease(addon))
	if err != nil {
		t.Fatalf("Acquire returned %v", err)
	}
	if !refused {
		t.Fatal("the test did not exercise the refused create")
	}
	if acquired {
		t.Fatal("the loser of the create race must not report the pair as its own")
	}
	if holder != "metrics" {
		t.Fatalf("holder = %q, want the winner of the race", holder)
	}
}

// TestAcquireReportsAConflictWhenTheTakeOverIsRejected pins the second race: two
// addons both find the claim orphaned and both try to take it over. The one whose
// update is rejected reports the pair as taken rather than failing the pass, and
// the reported holder is the one it read.
func TestAcquireReportsAConflictWhenTheTakeOverIsRejected(t *testing.T) {
	addon := claimingAddon("metrics", "podinfo")

	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(addon, claimFor("stable", "podinfo", "gone", "gone")).
		WithInterceptorFuncs(interceptor.Funcs{
			Update: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.UpdateOption) error {
				return apierrors.NewConflict(
					coordinationv1.Resource("leases"), obj.GetName(), errors.New("taken over first"),
				)
			},
		}).
		Build()
	claims := NewClaims(c, c, testNamespace)

	acquired, holder, err := claims.Acquire(context.Background(), adapter.NewAddonRelease(addon))
	if err != nil {
		t.Fatalf("Acquire returned %v, want the conflict reported as a taken pair", err)
	}
	if acquired {
		t.Fatal("a take-over the API server rejected must not be reported as acquired")
	}
	if holder != "gone" {
		t.Fatalf("holder = %q, want the holder this pass read", holder)
	}
}

// TestOwnedByAnswersFromTheLeaseHolder pins the read the webhook path depends on:
// ownership is the holder identity on the Lease, and a pair no one claimed is not
// owned by anyone rather than an error.
func TestOwnedByAnswersFromTheLeaseHolder(t *testing.T) {
	tests := []struct {
		name    string
		objects []client.Object
		want    bool
	}{
		{name: "no claim exists", want: false},
		{
			name:    "the claim is held by this addon",
			objects: []client.Object{claimFor("stable", "podinfo", "metrics", "metrics")},
			want:    true,
		},
		{
			name:    "the claim is held by another addon",
			objects: []client.Object{claimFor("stable", "podinfo", "other", "other")},
			want:    false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			addon := claimingAddon("metrics", "podinfo")
			claims, _ := newClaims(t, append(test.objects, addon)...)

			owned, err := claims.OwnedBy(context.Background(), adapter.NewAddonRelease(addon))
			if err != nil {
				t.Fatalf("OwnedBy returned %v", err)
			}
			if owned != test.want {
				t.Fatalf("OwnedBy = %v, want %v", owned, test.want)
			}
		})
	}
}

// TestDropFreesOnlyTheCallerOwnClaim pins the guard that makes Drop safe to
// call from any addon's deletion path: a duplicate that never acquired the pair
// must not free the claim of the addon that did.
func TestDropFreesOnlyTheCallerOwnClaim(t *testing.T) {
	owner := claimingAddon("metrics", "podinfo")
	duplicate := claimingAddon("metrics-copy", "podinfo")
	claims, c := newClaims(t, owner, duplicate, claimFor("stable", "podinfo", "metrics", "metrics"))

	if err := claims.Drop(context.Background(), adapter.NewAddonRelease(duplicate)); err != nil {
		t.Fatalf("Drop returned %v", err)
	}
	if got := claimHolder(t, c, "stable", "podinfo"); got != "metrics" {
		t.Fatalf("holder = %q, want the owner's claim left intact", got)
	}

	if err := claims.Drop(context.Background(), adapter.NewAddonRelease(owner)); err != nil {
		t.Fatalf("Drop returned %v", err)
	}

	lease := &coordinationv1.Lease{}
	key := client.ObjectKey{Namespace: testNamespace, Name: utils.GetChartClaimLeaseName("stable", "podinfo")}
	if err := c.Get(context.Background(), key, lease); !apierrors.IsNotFound(err) {
		t.Fatalf("getting the released lease returned %v, want it gone", err)
	}
}

// TestDropToleratesAMissingClaim pins that a deletion path which runs twice, or
// one whose claim was never created, reports no failure: the finalizer must not be
// held by a claim that is already gone.
func TestDropToleratesAMissingClaim(t *testing.T) {
	addon := claimingAddon("metrics", "podinfo")
	claims, _ := newClaims(t, addon)

	if err := claims.Drop(context.Background(), adapter.NewAddonRelease(addon)); err != nil {
		t.Fatalf("Drop returned %v, want a missing claim to be no failure", err)
	}
}

// TestDropLeavesAClaimTakenOverInBetween pins the precondition on the delete:
// between the read and the delete another addon may take the pair over, and the
// delete must name the revision that was read so the API server rejects it. The
// fake client does not enforce preconditions, so the interceptor stands in for the
// API server: it fails the test if the revision is missing, and answers the way the
// server answers a revision that no longer matches.
func TestDropLeavesAClaimTakenOverInBetween(t *testing.T) {
	addon := claimingAddon("metrics", "podinfo")
	held := claimFor("stable", "podinfo", "metrics", "metrics")

	var checked bool

	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(addon, held).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(_ context.Context, _ client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				options := &client.DeleteOptions{}
				for _, opt := range opts {
					opt.ApplyToDelete(options)
				}

				if options.Preconditions == nil || options.Preconditions.ResourceVersion == nil {
					t.Fatal("the delete must name the revision that was read")
				}

				checked = true

				return apierrors.NewConflict(
					coordinationv1.Resource("leases"), obj.GetName(), errors.New("taken over first"),
				)
			},
		}).
		Build()
	claims := NewClaims(c, c, testNamespace)

	if err := claims.Drop(context.Background(), adapter.NewAddonRelease(addon)); err != nil {
		t.Fatalf("Drop returned %v, want the rejected delete to be no failure", err)
	}
	if !checked {
		t.Fatal("the delete was never issued")
	}
	if got := claimHolder(t, c, "stable", "podinfo"); got != "metrics" {
		t.Fatalf("holder = %q, want the lease left where the conflict found it", got)
	}
}

// TestDropStaleDropsTheClaimsTheSpecMovedAwayFrom pins the cleanup a chart
// change leaves behind: the claim for the pair the addon now uses stays, the claims
// it moved away from go, and a pair another addon has since taken over is not
// touched even though the label still names this addon.
func TestDropStaleDropsTheClaimsTheSpecMovedAwayFrom(t *testing.T) {
	addon := claimingAddon("metrics", "podinfo")
	claims, c := newClaims(t,
		addon,
		claimFor("stable", "podinfo", "metrics", "metrics"),
		claimFor("stable", "old-chart", "metrics", "metrics"),
		claimFor("other-repo", "podinfo", "someone-else", "metrics"),
	)

	if err := claims.DropStale(context.Background(), adapter.NewAddonRelease(addon)); err != nil {
		t.Fatalf("DropStale returned %v", err)
	}

	if got := claimHolder(t, c, "stable", "podinfo"); got != "metrics" {
		t.Fatalf("holder of the current pair = %q, want it kept", got)
	}
	if got := claimHolder(t, c, "other-repo", "podinfo"); got != "someone-else" {
		t.Fatalf("holder of the taken-over pair = %q, want it left alone", got)
	}

	orphan := &coordinationv1.Lease{}
	key := client.ObjectKey{Namespace: testNamespace, Name: utils.GetChartClaimLeaseName("stable", "old-chart")}
	if err := c.Get(context.Background(), key, orphan); !apierrors.IsNotFound(err) {
		t.Fatalf("getting the orphaned lease returned %v, want it gone", err)
	}
}

// TestDropStaleIgnoresClaimsOfOtherAddons pins the label scope: the list is
// filtered by the addon's own source labels, so a claim belonging to a different
// addon is never a candidate for cleanup.
func TestDropStaleIgnoresClaimsOfOtherAddons(t *testing.T) {
	addon := claimingAddon("metrics", "podinfo")
	claims, c := newClaims(t,
		addon,
		claimFor("stable", "podinfo", "metrics", "metrics"),
		claimFor("other-repo", "other-chart", "another-addon", "another-addon"),
	)

	if err := claims.DropStale(context.Background(), adapter.NewAddonRelease(addon)); err != nil {
		t.Fatalf("DropStale returned %v", err)
	}

	if got := claimHolder(t, c, "other-repo", "other-chart"); got != "another-addon" {
		t.Fatalf("holder = %q, want another addon's claim untouched", got)
	}
}

// TestAcquireLeavesTheClaimOfALiveAddonThatMovedAway pins the claim against being
// handed out twice around an addon that moves off a chart and back.
//
// Reading the holder's spec to decide the Lease is an orphan cannot be made safe:
// the holder does not write to the Lease when it finds itself already recorded in
// it, so between that read and the takeover it can resume the pair without leaving
// a trace the optimistic update would collide with, and both addons end up believing
// they own one chart.
//
// The two passes below run one after the other rather than interleaved — the
// property under test is that a live holder is never dispossessed, which is what
// closes that window, and it holds whatever the order. The returning holder is
// passed the spec it has moved back to while the stored object still names the one
// it left, which is the state B would read in the window itself.
func TestAcquireLeavesTheClaimOfALiveAddonThatMovedAway(t *testing.T) {
	moved := claimingAddon("first", "other-chart")
	second := claimingAddon("second", "podinfo")
	claims, c := newClaims(t, moved, second, claimFor("stable", "podinfo", "first", "first"))

	// A is back on the pair before B writes: its own Acquire is a no-op on the
	// Lease, which is exactly why B cannot detect it.
	back := claimingAddon("first", "podinfo")
	acquired, holder, err := claims.Acquire(context.Background(), adapter.NewAddonRelease(back))
	if err != nil {
		t.Fatalf("Acquire for the returning holder returned %v", err)
	}
	if !acquired || holder != "first" {
		t.Fatalf("the recorded holder was refused its own claim: %v/%q", acquired, holder)
	}

	acquired, holder, err = claims.Acquire(context.Background(), adapter.NewAddonRelease(second))
	if err != nil {
		t.Fatalf("Acquire returned %v", err)
	}
	if acquired {
		t.Fatal("the pair was handed to a second addon while its holder was still there")
	}
	if holder != "first" {
		t.Fatalf("holder = %q, want the addon that holds the lease", holder)
	}
	if got := claimHolder(t, c, "stable", "podinfo"); got != "first" {
		t.Fatalf("holder on the lease = %q, want it untouched", got)
	}
}
