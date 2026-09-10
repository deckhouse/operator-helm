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

package release

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/opencontainers/go-digest"
	"github.com/werf/3p-fluxcd-pkg/chartutil"
	helmchartutil "helm.sh/helm/v3/pkg/chartutil"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
	"github.com/deckhouse/operator-helm/internal/manager/status"
	"github.com/deckhouse/operator-helm/internal/services"
	"github.com/deckhouse/operator-helm/internal/source"
	"github.com/deckhouse/operator-helm/internal/utils"
)

// internalResourceDeletionRequeueInterval bounds how often reconcileDelete
// re-checks whether the internal resources have finished being deleted. Watches
// on those resources drive most requeues; this is the safety net for a resource
// whose deletion is stuck and stops emitting events.
const internalResourceDeletionRequeueInterval = 30 * time.Second

// chartClaimConflictRequeueInterval bounds how often a release that lost the claim
// on its repository/chart pair re-checks whether the owner has released it. There
// is no watch on the claim Lease, so this periodic requeue is what lets a duplicate
// recover once the conflicting release is deleted or repointed at another chart.
const chartClaimConflictRequeueInterval = 30 * time.Second

// Deps are the collaborators of one release kind. The services are shared by every
// kind; the rest is what tells the families apart: how the API object is read
// (NewRelease), how its repository and catalog are found (Repositories), whether a
// repository/chart pair is claimed (Claim), whether the target namespace is created
// (Namespaces) and which identity the chart is applied with (Access).
type Deps struct {
	NewRelease   func() source.Release
	Repositories source.RepositoryResolver
	Chart        *services.ChartService
	OCI          *services.OCIRepoService
	Release      *services.ReleaseService
	Maintenance  *services.MaintenanceService
	Claim        source.ChartClaim
	Namespaces   source.TargetNamespaceEnsurer
	Access       source.AccessManager
	Status       *status.Manager
}

func New(c client.Client, deps Deps) *Reconciler {
	return &Reconciler{Client: c, deps: deps}
}

type Reconciler struct {
	client.Client

	deps Deps
}

func (r *Reconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	logger := log.FromContext(ctx)
	ctx = log.IntoContext(ctx, logger)

	rel := r.deps.NewRelease()
	if err := r.Get(ctx, req.NamespacedName, rel.Object()); err != nil {
		if apierrors.IsNotFound(err) {
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, fmt.Errorf("getting release: %w", err)
	}

	if !rel.Object().GetDeletionTimestamp().IsZero() {
		return r.reconcileDelete(ctx, rel)
	}

	// Claim the repository/chart pair before anything else, including adding the
	// finalizer. The claim is the authoritative, race-free guard on uniqueness (the
	// webhook only fast-rejects the obvious duplicate on CREATE and cannot stop
	// concurrent creates from racing past it). It must run before the finalizer
	// because a duplicate that loses the race must not accrue a finalizer it would
	// otherwise have to clean up: it simply surfaces the conflict on its status and
	// requeues, recovering on its own once the owner releases the pair.
	acquired, holder, err := r.deps.Claim.Acquire(ctx, rel)
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("acquiring chart claim: %w", err)
	}
	if !acquired {
		return reconcile.Result{RequeueAfter: chartClaimConflictRequeueInterval}, r.deps.Status.Update(
			ctx, rel.Object(), status.NoopStatusMutator, status.NoopStatusMapper,
			services.ReleaseResult{Status: status.Failed(
				rel.Object(),
				helmv1alpha1.ReasonChartClaimConflict,
				fmt.Sprintf("chart %q is already used by %s/%s", rel.ChartRef().Chart, strings.ToLower(rel.Kind()), holder),
				nil,
			)},
		)
	}

	if utils.IsSystemNamespace(rel.TargetNamespace()) {
		return reconcile.Result{}, r.deps.Status.Update(ctx, rel.Object(), status.NoopStatusMutator, status.NoopStatusMapper, services.ReleaseResult{Status: status.Failed(
			rel.Object(),
			helmv1alpha1.ReasonFailed,
			"Target namespace cannot be a system namespace",
			fmt.Errorf("target namespace %q is a system namespace", rel.TargetNamespace()),
		)})
	}

	if !controllerutil.ContainsFinalizer(rel.Object(), helmv1alpha1.FinalizerName) {
		controllerutil.AddFinalizer(rel.Object(), helmv1alpha1.FinalizerName)
		if err := r.Update(ctx, rel.Object()); err != nil {
			return reconcile.Result{}, fmt.Errorf("adding finalizer: %w", err)
		}
		// Continue reconciling in the same pass: adding a finalizer is a
		// metadata-only change that does not bump generation, so the resulting
		// update event is dropped by the generation/annotation predicates and
		// would not trigger a follow-up reconcile.
	}

	if err := r.deps.Claim.ReleaseStale(ctx, rel); err != nil {
		return reconcile.Result{}, fmt.Errorf("releasing stale chart claims: %w", err)
	}

	if r.deps.Maintenance.IsMaintenanceModeChangeRequired(rel) {
		maintenanceRes := r.deps.Maintenance.EnsureMaintenanceMode(ctx, rel)
		if err := r.deps.Status.Update(ctx, rel.Object(), status.NoopStatusMutator, status.NoopStatusMapper, maintenanceRes, status.AsCondition(maintenanceRes, "Ready")); err != nil {
			return reconcile.Result{}, err
		}

		if !rel.MaintenanceActivated() {
			// Maintenance is being lifted: a pending force request is about to become
			// actionable, so it is left in place for the pass that can honour it.
			return reconcile.Result{}, nil
		}

		return reconcile.Result{}, r.discardForceReconcile(ctx, rel)
	}

	if rel.MaintenanceActivated() {
		return reconcile.Result{}, r.discardForceReconcile(ctx, rel)
	}

	repo, catalog, err := r.deps.Repositories.Resolve(ctx, rel.ChartRef().Repository)
	if err != nil {
		return reconcile.Result{}, r.deps.Status.Update(ctx, rel.Object(), status.NoopStatusMutator, status.NoopStatusMapper, services.ReleaseResult{Status: status.Failed(
			rel.Object(),
			helmv1alpha1.ReasonFailed,
			"Failed to get internal repository",
			fmt.Errorf("getting internal repository: %w", err),
		)})
	}

	repoType, err := utils.GetRepositoryType(repo.URL())
	if err != nil {
		return reconcile.Result{}, r.deps.Status.Update(ctx, rel.Object(), status.NoopStatusMutator, status.NoopStatusMapper, services.ReleaseResult{Status: status.Failed(
			rel.Object(),
			helmv1alpha1.ReasonFailed,
			fmt.Sprintf("Failed to parse repository type: %s", err.Error()),
			err,
		)})
	}

	if err := r.deps.Namespaces.EnsureTargetNamespace(ctx, rel); err != nil {
		return reconcile.Result{}, r.deps.Status.Update(ctx, rel.Object(), status.NoopStatusMutator, status.NoopStatusMapper, services.ReleaseResult{Status: status.Failed(
			rel.Object(),
			helmv1alpha1.ReasonFailed,
			fmt.Sprintf("Failed to reconcile target namespace: %s", err.Error()),
			err,
		)})
	}

	// The identity comes before the internal sources: helm-controller checks that
	// the account named on the HelmRelease exists before it impersonates it, so a
	// HelmRelease created ahead of its ServiceAccount would fail its first pass.
	if err := r.deps.Access.EnsureAccess(ctx, rel); err != nil {
		return reconcile.Result{}, r.deps.Status.Update(ctx, rel.Object(), status.NoopStatusMutator, status.NoopStatusMapper, services.ReleaseResult{Status: status.Failed(
			rel.Object(),
			helmv1alpha1.ReasonAccessSetupFailed,
			fmt.Sprintf("Failed to set up the release identity: %s", err.Error()),
			err,
		)})
	}

	// From here on every path reaches the status update at the end of the pass,
	// which is what consumes the force request. Marking earlier would leave the
	// progress condition behind on a validation failure that never consumes it.
	forced := rel.ForceReconcileRequired()
	if forced {
		if err := r.markForceReconcileInProgress(ctx, rel); err != nil {
			return reconcile.Result{}, err
		}
	}

	var chartRes services.ChartResult
	var repoRes services.OCIRepoResult
	var releaseRes services.ReleaseResult

	_, chartVersion, chartErr := r.getChartVersion(ctx, catalog, repo, rel, repoType)

	// The source is resolved once, before the branches: which internal object a
	// release needs is a property of the version it asks for, and a version whose
	// source cannot be resolved is as unusable as a version that is missing.
	var src utils.ChartSource
	if chartErr == nil {
		src, chartErr = utils.ResolveChartSource(repo.URL(), chartVersion)
	}

	names := rel.InternalNames()

	switch {
	case chartErr != nil:
		// One report for both branches: until the source is known, neither internal
		// object may be touched, and which one would have been touched is precisely
		// what could not be determined.
		chartRes = services.ChartResult{Status: status.Failed(
			rel.Object(),
			helmv1alpha1.ReasonChartFetchFailed,
			"Failed to resolve the desired chart version",
			chartErr,
		)}
	case src.Kind == utils.InternalHelmRepository:
		// The version may have moved out of a registry — either because the user
		// repointed the repository, or because the index re-published it as an
		// archive. Either way the internal OCIRepository is no longer the source.
		superseded, err := r.deps.OCI.RemoveOCIRepository(ctx, names)
		if err != nil {
			chartRes = services.ChartResult{
				Status: status.Failed(rel.Object(), helmv1alpha1.ReasonFailed, "Repository change failed", err),
			}

			break
		}

		r.logSourceKindFlip(ctx, rel, src.Kind, superseded != nil)

		chartRes = r.deps.Chart.EnsureHelmChart(ctx, rel, repo)
	case src.Kind == utils.InternalOCIRepository:
		superseded, err := r.deps.Chart.CleanupHelmChart(ctx, names)
		if err != nil {
			chartRes = services.ChartResult{
				Status: status.Failed(rel.Object(), helmv1alpha1.ReasonFailed, "Repository change failed", err),
			}

			break
		}

		r.logSourceKindFlip(ctx, rel, src.Kind, superseded != nil)

		repoRes = r.deps.OCI.EnsureInternalOCIRepository(ctx, rel, repo, src, chartVersion)
	default:
		return reconcile.Result{}, r.deps.Status.Update(ctx, rel.Object(), status.NoopStatusMutator, status.NoopStatusMapper, services.ReleaseResult{Status: status.Failed(
			rel.Object(),
			helmv1alpha1.ReasonFailed,
			fmt.Sprintf("Unsupported chart source: %s", src.Kind),
			fmt.Errorf("unsupported chart source: %s", src.Kind),
		)})
	}

	if chartRes.HasArtifact() || repoRes.HasArtifact() {
		var artifactRevision string
		switch src.Kind {
		case utils.InternalHelmRepository:
			if chartRes.Artifact != nil {
				artifactRevision = chartRes.Artifact.Revision
			}
		case utils.InternalOCIRepository:
			if repoRes.Artifact != nil {
				artifactRevision = repoRes.Artifact.Revision
			}
		}

		releaseRes = r.deps.Release.EnsureHelmRelease(ctx, rel, src.Kind, artifactRevision)
	}

	if err := r.deps.Status.Update(
		ctx,
		rel.Object(),
		setStatusAttrs(rel, src.Kind, chartRes, repoRes, releaseRes, forceReconcileOutcome{
			forced: forced,
			now:    time.Now().UTC(),
		}),
		status.NoopStatusMapper,
		chartRes,
		repoRes,
		releaseRes,
	); client.IgnoreNotFound(err) != nil {
		return reconcile.Result{}, fmt.Errorf("failed to update status: %w", err)
	}

	// The annotation is consumed after the status patch, so a conflict on the patch
	// leaves the request in place to be retried rather than losing it.
	if err := r.reconcileForceAnnotation(ctx, req.NamespacedName); err != nil {
		return reconcile.Result{}, fmt.Errorf("failed to reconcile force annotation: %w", err)
	}

	// A probe that could not reach the registry asks for another pass: there is no
	// watch that fires when a foreign registry starts answering again.
	return reconcile.Result{RequeueAfter: repoRes.RequeueAfter}, nil
}

func (r *Reconciler) reconcileDelete(ctx context.Context, rel source.Release) (reconcile.Result, error) {
	logger := log.FromContext(ctx)

	if !controllerutil.ContainsFinalizer(rel.Object(), helmv1alpha1.FinalizerName) {
		return reconcile.Result{}, nil
	}

	names := rel.InternalNames()

	// The finalizer must stay until the internal resources are actually gone.
	// A Delete only sets a deletion timestamp; the downstream controllers keep
	// their finalizers until they finish tearing the underlying release/source
	// down. Removing our finalizer earlier would delete the release object and
	// orphan a HelmRelease that helm-controller never managed to uninstall.
	//
	// The release is uninstalled first; only once it is gone do we remove the
	// chart/repository sources it referenced, and only after those the identity
	// helm-controller uninstalled with. Each step waits for the resource to
	// actually disappear and surfaces the blocking resource's readiness on the
	// release object so the reason a deletion stalls is observable.
	release, err := r.deps.Release.CleanupHelmRelease(ctx, names)
	if err != nil {
		return reconcile.Result{}, err
	}
	if release != nil {
		// The release object is a facade over the HelmRelease: a bad spec parameter
		// that blocks helm uninstall is propagated into the release. Keep re-applying
		// the (possibly corrected) spec to the still-present release so the
		// uninstall can be fixed via the release object even while it is being deleted.
		if err := r.deps.Release.SyncReleaseSpec(ctx, rel, release); err != nil {
			return reconcile.Result{}, err
		}
		return r.awaitInternalResourceDeletion(ctx, rel, "internal release", release)
	}

	chart, err := r.deps.Chart.CleanupHelmChart(ctx, names)
	if err != nil {
		return reconcile.Result{}, err
	}
	if chart != nil {
		return r.awaitInternalResourceDeletion(ctx, rel, "internal chart", chart)
	}

	ociRepo, err := r.deps.OCI.RemoveOCIRepository(ctx, names)
	if err != nil {
		return reconcile.Result{}, err
	}
	if ociRepo != nil {
		return r.awaitInternalResourceDeletion(ctx, rel, "internal repository", ociRepo)
	}

	// The identity goes last: helm-controller uninstalls as that account, so it
	// has to outlive the HelmRelease.
	if err := r.deps.Access.CleanupAccess(ctx, rel); err != nil {
		return reconcile.Result{}, fmt.Errorf("cleaning up release identity: %w", err)
	}

	// Release the claim only once every downstream resource is gone: releasing it
	// earlier would let another release start reconciling the same chart while this
	// one's HelmRelease is still being uninstalled — exactly the collision the claim
	// prevents.
	if err := r.deps.Claim.Release(ctx, rel); err != nil {
		return reconcile.Result{}, fmt.Errorf("releasing chart claim: %w", err)
	}

	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latest := r.deps.NewRelease()
		if err := r.Get(ctx, client.ObjectKeyFromObject(rel.Object()), latest.Object()); err != nil {
			return client.IgnoreNotFound(err)
		}

		if controllerutil.RemoveFinalizer(latest.Object(), helmv1alpha1.FinalizerName) {
			if err := r.Update(ctx, latest.Object()); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return reconcile.Result{}, fmt.Errorf("removing finalizer: %w", err)
	}

	logger.Info("Cleanup complete")

	return reconcile.Result{}, nil
}

// awaitInternalResourceDeletion surfaces that an internal resource is still being
// deleted on the release's status (via the shared status manager) and requeues
// without removing the finalizer. The resource name is kept abstract so its
// internal type is not leaked to the user.
func (r *Reconciler) awaitInternalResourceDeletion(ctx context.Context, rel source.Release, name string, resource status.DeletingResource) (reconcile.Result, error) {
	log.FromContext(ctx).Info("Waiting for internal resource to be deleted before removing finalizer", "resource", name)

	if err := r.deps.Status.MarkUninstallPending(ctx, rel.Object(), name, resource); client.IgnoreNotFound(err) != nil {
		return reconcile.Result{}, fmt.Errorf("updating deletion status: %w", err)
	}

	return reconcile.Result{RequeueAfter: internalResourceDeletionRequeueInterval}, nil
}

// markForceReconcileInProgress publishes Reconciling before the work a force
// request asks for begins. A forced pass is the one case where someone is
// watching: they annotated the object a moment ago and want to see it was picked
// up. The condition is removed again by the status update that ends the pass.
func (r *Reconciler) markForceReconcileInProgress(ctx context.Context, rel source.Release) error {
	err := r.deps.Status.PatchStatus(ctx, rel.Object(), func() {
		apimeta.SetStatusCondition(rel.Object().GetConditions(), metav1.Condition{
			Type:               helmv1alpha1.ConditionTypeReconciling,
			Status:             metav1.ConditionTrue,
			Reason:             helmv1alpha1.ReasonForceReconcile,
			Message:            "Forced reconciliation in progress",
			ObservedGeneration: rel.Generation(),
		})
	})
	if client.IgnoreNotFound(err) != nil {
		return fmt.Errorf("publishing forced reconciliation progress: %w", err)
	}

	return nil
}

// discardForceReconcile drops the in-flight force state from a release that is
// entering, or already sitting in, maintenance mode. Every pass on such a release
// returns before the work a force request asks for, so the request can never be
// acted on: leaving Reconciling behind would report work in flight to kstatus
// forever, and leaving the annotation would replay a request made days earlier the
// moment maintenance is lifted. Reconciling is removed unconditionally because the
// force path is its only producer on a release.
//
// lastForceReconcileTime is deliberately untouched — the request was discarded,
// not processed, and the stamp means the latter.
func (r *Reconciler) discardForceReconcile(ctx context.Context, rel source.Release) error {
	err := r.deps.Status.PatchStatus(ctx, rel.Object(), func() {
		apimeta.RemoveStatusCondition(rel.Object().GetConditions(), helmv1alpha1.ConditionTypeReconciling)
	})
	if client.IgnoreNotFound(err) != nil {
		return fmt.Errorf("dropping forced reconciliation progress: %w", err)
	}

	if err := r.reconcileForceAnnotation(ctx, client.ObjectKeyFromObject(rel.Object())); err != nil {
		return fmt.Errorf("failed to reconcile force annotation: %w", err)
	}

	return nil
}

func (r *Reconciler) reconcileForceAnnotation(ctx context.Context, key client.ObjectKey) error {
	rel := r.deps.NewRelease()

	if err := r.Get(ctx, key, rel.Object()); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("getting release: %w", err)
	}

	annotations := rel.Object().GetAnnotations()
	if _, found := annotations[helmv1alpha1.AnnotationForceReconcile]; !found {
		// Guard on the annotation itself, not on the map: a release carrying any
		// unrelated annotation would otherwise take an empty PATCH on every pass.
		return nil
	}

	patchBase := client.MergeFrom(rel.Object().DeepCopyObject().(client.Object))

	delete(annotations, helmv1alpha1.AnnotationForceReconcile)
	rel.Object().SetAnnotations(annotations)

	if err := r.Patch(ctx, rel.Object(), patchBase); err != nil {
		return fmt.Errorf("removing force reconcile annotation: %w", err)
	}

	return nil
}

// getChartVersion resolves the catalog entry for the version the release asks for
// and rejects an entry that cannot be deployed. Two things make an entry unusable:
// an index reference that cannot be addressed, and — for a version of an oci://
// repository — a missing media type, which is exactly "the catalog does not yet
// know enough to build the internal OCIRepository". A version published in a
// registry by a helm index carries no media type by design: its artifact is examined
// at deploy time, so the second rule does not apply to it.
//
// A version retained after its tag disappeared keeps both its media type and its
// reference, so this gate stays open for it and the release keeps reconciling
// everything else — its values, its maintenance mode, its removal.
func (r *Reconciler) getChartVersion(
	ctx context.Context,
	catalog source.Catalog,
	repo source.Repository,
	rel source.Release,
	repoType utils.InternalRepositoryType,
) (client.Object, *helmv1alpha1.ChartVersion, error) {
	ref := rel.ChartRef()

	obj, catalogStatus, err := catalog.Lookup(ctx, repo, ref.Chart)
	if err != nil {
		return nil, nil, fmt.Errorf("getting chart catalog entry: %w", err)
	}

	for i := range catalogStatus.Versions {
		version := &catalogStatus.Versions[i]
		if version.Version != ref.Version {
			continue
		}

		if version.UnavailableReason == helmv1alpha1.UnavailableReasonInvalidChartReference {
			// The index publishes this version in a registry at a reference that
			// cannot be addressed. Without this the version would fall back to the
			// helm path and fail on the very same url, reported by the source
			// controller as an opaque fetch error.
			return nil, nil, fmt.Errorf(
				"chart version %q cannot be deployed: %s",
				version.Version, versionUnavailableDetail(*version),
			)
		}

		if repoType == utils.InternalOCIRepository && version.OCIRef == "" && version.MediaType == "" {
			return nil, nil, fmt.Errorf(
				"chart version %q cannot be deployed: %s",
				version.Version, versionUnavailableDetail(*version),
			)
		}

		return obj, version, nil
	}

	return nil, nil, fmt.Errorf("chart catalog does not have version %q", ref.Version)
}

// versionUnavailableDetail explains why a catalog entry is not deployable.
func versionUnavailableDetail(version helmv1alpha1.ChartVersion) string {
	switch {
	case version.UnavailableReason == "":
		return "the repository catalog has not resolved it yet"
	case version.UnavailableMessage == "":
		return version.UnavailableReason
	default:
		return version.UnavailableReason + ": " + version.UnavailableMessage
	}
}

// logSourceKindFlip reports that the same chart version changed where it is
// published: the repository index moved it between a chart archive and a registry.
// Nothing else surfaces that — status records the applied version but not the source
// it came from — and it upgrades a running release nobody asked to upgrade, so it has
// to be findable in the log. superseded says whether an internal source of the other
// kind was actually removed in this pass.
func (r *Reconciler) logSourceKindFlip(
	ctx context.Context,
	rel source.Release,
	kind utils.InternalRepositoryType,
	superseded bool,
) {
	if !superseded {
		return
	}

	last := rel.LastAppliedChart()
	if last == nil || *last != rel.ChartRef() {
		// Not a flip: the release is moving to another version (or another chart),
		// and the superseded source belonged to the one it is leaving. The whole ref
		// has to match: LastAppliedChart carries its own repository/chart identity
		// and can lag behind the spec when the release is repointed at a different
		// chart, so the version alone could match by coincidence while naming an
		// entirely different chart's history.
		return
	}

	log.FromContext(ctx).Info(
		"Chart version changed where it is published; the running release will be upgraded from the new source",
		"version", rel.ChartRef().Version,
		"source", kind,
	)
}

// forceReconcileOutcome carries what the status mutator needs to close out a
// forced pass. It is a struct so the clock stays with the caller: the mutator
// runs inside the status manager, after it has snapshotted the object it diffs
// against, which is the only place a change to the status is actually patched.
type forceReconcileOutcome struct {
	forced bool
	now    time.Time
}

// setStatusAttrs writes the fields of the status the conditions do not cover. It
// closes over rel rather than asserting the object's type: rel.Object() is the very
// object the status manager hands back, so the writes land on it.
func setStatusAttrs(
	rel source.Release,
	sourceKind utils.InternalRepositoryType,
	chartRes services.ChartResult,
	repoRes services.OCIRepoResult,
	releaseRes services.ReleaseResult,
	force forceReconcileOutcome,
) status.MutatorFunc {
	return func(obj status.ObjectWithConditions, results []status.Provider) (status.ObjectWithConditions, []status.Provider) {
		results = status.DetermineConditions(obj, results...)

		if force.forced {
			// The stamp records that the request was acted on, not that it succeeded:
			// the outcome is reported by Ready. Reconciling is removed explicitly —
			// the status manager only ever sets conditions.
			rel.SetLastForceReconcileTime(metav1.Time{Time: force.now})
			apimeta.RemoveStatusCondition(rel.Object().GetConditions(), helmv1alpha1.ConditionTypeReconciling)
		}

		latestRelease := releaseRes.History.Latest()

		var updateChart bool

		switch sourceKind {
		case utils.InternalHelmRepository:
			if chartRes.HasArtifact() && releaseRes.IsReady() && rel.IsChartStatusInfoOutdated() {
				updateChart = true
			}
		case utils.InternalOCIRepository:
			if repoRes.HasArtifact() && releaseRes.IsReady() && rel.IsChartStatusInfoOutdated() {
				updateChart = true
			}
		}

		if updateChart {
			rel.SetLastAppliedChart(rel.ChartRef())
		}

		if releaseRes.IsReady() && latestRelease != nil {
			rawValues := []byte(`{}`)
			if rel.Values() != nil {
				rawValues = rel.Values().Raw
			}

			values, _ := helmchartutil.ReadValues(rawValues)
			if latestRelease.Status == "deployed" && latestRelease.ConfigDigest == chartutil.DigestValues(digest.Canonical, values).String() {
				if rel.Values() == nil {
					rel.SetLastAppliedValues(nil)
				} else {
					rel.SetLastAppliedValues(rel.Values().DeepCopy())
				}
			}
		}

		return obj, results
	}
}
