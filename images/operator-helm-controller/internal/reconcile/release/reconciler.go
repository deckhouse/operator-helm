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

	"github.com/fluxcd/pkg/chartutil"
	"github.com/opencontainers/go-digest"
	helmcommon "helm.sh/helm/v4/pkg/chart/common"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
	"github.com/deckhouse/operator-helm/internal/chartsource"
	"github.com/deckhouse/operator-helm/internal/contract"
	"github.com/deckhouse/operator-helm/internal/reconcile/pass"
	"github.com/deckhouse/operator-helm/internal/status"
	"github.com/deckhouse/operator-helm/internal/utils"
)

// chartClaimConflictRequeueInterval bounds how often a release that lost the claim
// on its repository/chart pair re-checks whether the owner has released it. There
// is no watch on the claim Lease, so this periodic requeue is what lets a duplicate
// recover once the conflicting release is deleted or repointed at another chart.
const chartClaimConflictRequeueInterval = 30 * time.Second

// Deps are the collaborators of one release kind. Their implementations are shared
// by every kind; the rest is what tells the families apart: how the API object is
// read (NewRelease), how its repository and catalog are found (Repositories),
// whether a repository/chart pair is claimed (Claims), whether the target namespace
// is created (Namespaces) and which identity the chart is applied with (Identity).
type Deps struct {
	NewRelease      func() contract.Release
	Repositories    Repositories
	Charts          Charts
	OCIRepositories OCIRepositories
	Releases        Releases
	Maintenance     Maintenance
	Claims          Claims
	Namespaces      Namespaces
	Identity        Identity
	Status          *status.Manager
}

func New(c client.Client, deps Deps) *Reconciler {
	return &Reconciler{Client: c, deps: deps}
}

type Reconciler struct {
	client.Client

	deps Deps
}

func (r *Reconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
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

	in := Inputs{
		Generation:         rel.Generation(),
		ObservedGeneration: rel.Object().GetObservedGeneration(),
		Now:                time.Now().UTC(),
		ConditionTypes:     rel.Object().GetConditionTypesForUpdate(),
	}

	// Claim the repository/chart pair before anything else, including adding the
	// finalizer. The claim is the authoritative, race-free guard on uniqueness (the
	// webhook only fast-rejects the obvious duplicate on CREATE and cannot stop
	// concurrent creates from racing past it). It must run before the finalizer
	// because a duplicate that loses the race must not accrue a finalizer it would
	// otherwise have to clean up: it simply surfaces the conflict on its status and
	// requeues, recovering on its own once the owner releases the pair.
	acquired, holder, err := r.deps.Claims.Acquire(ctx, rel)
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("acquiring chart claim: %w", err)
	}
	if !acquired {
		return r.finish(ctx, rel, in, StepFailed{Failure{
			Reason: helmv1alpha1.ReasonChartClaimConflict,
			Message: fmt.Sprintf("chart %q is already used by %s/%s",
				rel.ChartRef().Chart, strings.ToLower(rel.Kind()), holder),
			// No watch fires when the owner lets the pair go, so the recovery of a
			// duplicate rides on this timer alone.
			RequeueAfter: chartClaimConflictRequeueInterval,
		}})
	}

	if utils.IsSystemNamespace(rel.TargetNamespace()) {
		return r.finish(ctx, rel, in, StepFailed{Failure{
			Reason:   helmv1alpha1.ReasonFailed,
			Message:  "Target namespace cannot be a system namespace",
			Err:      fmt.Errorf("target namespace %q is a system namespace", rel.TargetNamespace()),
			Terminal: true,
		}})
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

	if err := r.deps.Claims.DropStale(ctx, rel); err != nil {
		return reconcile.Result{}, fmt.Errorf("releasing stale chart claims: %w", err)
	}

	if r.deps.Maintenance.ChangeRequired(rel) {
		if err := r.markMaintenanceChangeInProgress(ctx, rel); err != nil {
			return reconcile.Result{}, err
		}

		result := r.deps.Maintenance.Ensure(ctx, rel)
		if result.Err != nil || result.Activated {
			return r.finish(ctx, rel, in, MaintenanceChanged{Result: result})
		}

		// Maintenance was lifted, so the release is managed again and this pass is the
		// one that has to make that true: ending here would leave a release reported as
		// managed with no chart deployed and nothing scheduled to deploy it. The lift
		// itself is carried to Evaluate, which owns the Managed verdict.
		in.MaintenanceLifted = true
	}

	if rel.MaintenanceActivated() {
		return r.finish(ctx, rel, in, InMaintenance{})
	}

	repo, catalog, err := r.deps.Repositories.Resolve(ctx, rel.ChartRef().Repository)
	if err != nil {
		return r.finish(ctx, rel, in, StepFailed{Failure{
			Reason:  helmv1alpha1.ReasonFailed,
			Message: "Failed to get internal repository",
			Err:     fmt.Errorf("getting internal repository: %w", err),
			// A repository that is not there yet is the one failure of this step the
			// release is woken from: the repository kinds are watched, so the pass runs
			// again the moment one is created. Anything else — a rejected or failed read —
			// is a passing fault of the API server with nothing to wake the release, so it
			// goes to the work queue.
			AwaitsWatch: apierrors.IsNotFound(err),
		}})
	}

	repoType, err := chartsource.KindOf(repo.URL())
	if err != nil {
		// The repository's own url is what cannot be read, and the repository reports
		// the same fault as Stalled. Retrying here would only rediscover it; the
		// release comes back when the repository's generation changes, which is what
		// correcting the url does.
		return r.finish(ctx, rel, in, StepFailed{Failure{
			Reason:   helmv1alpha1.ReasonUnsupportedRepositoryType,
			Message:  fmt.Sprintf("Failed to parse repository type: %s", err.Error()),
			Err:      err,
			Terminal: true,
		}})
	}

	if err := r.deps.Namespaces.EnsureTarget(ctx, rel); err != nil {
		return r.finish(ctx, rel, in, StepFailed{Failure{
			Reason:  helmv1alpha1.ReasonFailed,
			Message: fmt.Sprintf("Failed to reconcile target namespace: %s", err.Error()),
			Err:     err,
		}})
	}

	// The identity comes before the internal sources: helm-controller checks that
	// the account named on the HelmRelease exists before it impersonates it, so a
	// HelmRelease created ahead of its ServiceAccount would fail its first pass.
	// The watches on the Role and the RoleBinding only fire on a write that landed,
	// so a step that failed before writing anything comes back through the work
	// queue's rate limiter and nothing else. A terminal failure is the exception: the
	// object in the way carries no managed-by label, so those watches never see it go
	// either — only a force request or an edit to the release gets this pass run again.
	if identity := r.deps.Identity.Ensure(ctx, rel); identity.Err != nil {
		return r.finish(ctx, rel, in, StepFailed{Failure{
			Reason:   identity.Reason,
			Message:  identity.Message,
			Err:      identity.Err,
			Terminal: identity.Terminal,
		}})
	}

	// From here on every path reaches finish, which is what consumes the force
	// request. Marking earlier would leave the progress condition behind on a
	// validation failure that never consumes it.
	forced := rel.ForceReconcileRequired()
	if forced {
		if err := r.markForceReconcileInProgress(ctx, rel); err != nil {
			return reconcile.Result{}, err
		}
	}

	return r.finish(ctx, rel, in, r.reconcileChart(ctx, rel, repo, catalog, repoType, forced))
}

// reconcileChart carries the pass to the internal objects: it resolves the version
// and the source it is served from, brings that internal source to the state the
// release asks for and, once the source has an artifact, the internal HelmRelease.
// Nothing here is handed to the work queue — every failure it meets is part of the
// outcome the release reports.
func (r *Reconciler) reconcileChart(
	ctx context.Context,
	rel contract.Release,
	repo contract.Repository,
	catalog contract.Catalog,
	repoType chartsource.Kind,
	forced bool,
) Outcome {
	src, chartVersion, failure := r.resolveSource(ctx, rel, repo, catalog, repoType)

	outcome := ChartReached{
		Forced:            forced,
		ChartInfoOutdated: rel.IsChartStatusInfoOutdated(),
		ValuesDigest:      valuesDigest(rel),
	}

	switch {
	case failure != nil:
		outcome.Internal = SourceUnresolved{Failure: *failure}
	case src.Kind == chartsource.Helm:
		outcome.Internal = r.serveFromHelmChart(ctx, rel, repo, src.Kind)
	case src.Kind == chartsource.OCI:
		outcome.Internal = r.serveFromOCIRepository(ctx, rel, repo, src, chartVersion)
	default:
		return StepFailed{Failure{
			Reason:   helmv1alpha1.ReasonFailed,
			Message:  fmt.Sprintf("Unsupported chart source: %s", src.Kind),
			Err:      fmt.Errorf("unsupported chart source: %s", src.Kind),
			Terminal: true,
		}}
	}

	if revision, ok := artifactRevision(outcome.Internal); ok {
		result := r.deps.Releases.Ensure(ctx, rel, src.Kind, revision)
		outcome.Release = &result
	}

	return outcome
}

// resolveSource is the version the release asks for and the source it is installed
// from. The source is resolved once, before the branches: which internal object a
// release needs is a property of the version it asks for, and a version whose source
// cannot be resolved is as unusable as a version that is missing. One report covers
// both, because until the source is known, neither internal object may be touched,
// and which one would have been touched is precisely what could not be determined.
func (r *Reconciler) resolveSource(
	ctx context.Context,
	rel contract.Release,
	repo contract.Repository,
	catalog contract.Catalog,
	repoType chartsource.Kind,
) (chartsource.Source, *helmv1alpha1.ChartVersion, *Failure) {
	_, chartVersion, err := r.getChartVersion(ctx, catalog, repo, rel, repoType)

	var src chartsource.Source
	if err == nil {
		src, err = chartsource.Resolve(repo.URL(), chartVersion)
	}

	if err != nil {
		return src, chartVersion, &Failure{
			Reason:  helmv1alpha1.ReasonChartFetchFailed,
			Message: "Failed to resolve the desired chart version",
			Err:     err,
		}
	}

	return src, chartVersion, nil
}

// serveFromHelmChart takes the internal OCIRepository away before bringing up the
// internal HelmChart. The version may have moved out of a registry — either because
// the user repointed the repository, or because the index re-published it as an
// archive — and either way the internal OCIRepository is no longer the source.
func (r *Reconciler) serveFromHelmChart(
	ctx context.Context,
	rel contract.Release,
	repo contract.Repository,
	kind chartsource.Kind,
) InternalSource {
	superseded, err := r.deps.OCIRepositories.Remove(ctx, rel.InternalNames())
	if err != nil {
		return SourceUnresolved{Failure: repositoryChangeFailure(err)}
	}

	r.logSourceKindFlip(ctx, rel, kind, superseded != nil)

	return ServedByHelmChart{Result: r.deps.Charts.Ensure(ctx, rel, repo)}
}

func (r *Reconciler) serveFromOCIRepository(
	ctx context.Context,
	rel contract.Release,
	repo contract.Repository,
	src chartsource.Source,
	chartVersion *helmv1alpha1.ChartVersion,
) InternalSource {
	superseded, err := r.deps.Charts.Cleanup(ctx, rel.InternalNames())
	if err != nil {
		return SourceUnresolved{Failure: repositoryChangeFailure(err)}
	}

	r.logSourceKindFlip(ctx, rel, src.Kind, superseded != nil)

	return ServedByOCIRepository{Result: r.deps.OCIRepositories.Ensure(ctx, rel, repo, src, chartVersion)}
}

// finish applies the decision and consumes the force annotation. The annotation is
// removed after the status patch so a conflict does not lose the request.
func (r *Reconciler) finish(
	ctx context.Context,
	rel contract.Release,
	in Inputs,
	outcome Outcome,
) (reconcile.Result, error) {
	in.Outcome = outcome
	decision := Evaluate(in)

	if decision.Reported != nil {
		// Only the identity failure is handed to the work queue, which logs it on the
		// way past; every other failure ends the pass quietly, so this is the one
		// place its cause is written down.
		log.FromContext(ctx).Error(decision.Reported.Err, decision.Reported.Message,
			"reason", decision.Reported.Reason)
	}

	if err := r.deps.Status.PatchStatus(ctx, rel.Object(), func() {
		applyDecision(rel, decision)
	}); client.IgnoreNotFound(err) != nil {
		return reconcile.Result{}, fmt.Errorf("failed to update status: %w", err)
	}

	if decision.ConsumeForce {
		if err := pass.ConsumeForceAnnotation(ctx, r.Client, client.ObjectKeyFromObject(rel.Object()), r.deps.NewRelease().Object()); err != nil {
			return reconcile.Result{}, fmt.Errorf("failed to reconcile force annotation: %w", err)
		}
	}

	return reconcile.Result{RequeueAfter: decision.RequeueAfter}, decision.Err
}

// applyDecision writes the evaluated status onto the release. Conditions are merged
// rather than replaced: a pass reports on the steps it ran, and the verdicts of the
// steps it did not run stay where they are.
func applyDecision(rel contract.Release, decision Decision) {
	conditions := rel.Object().GetConditions()

	for _, condition := range decision.Conditions {
		apimeta.SetStatusCondition(conditions, condition)
	}

	for _, conditionType := range decision.RemoveConditions {
		apimeta.RemoveStatusCondition(conditions, conditionType)
	}

	if decision.ObservedGeneration != nil {
		rel.Object().SetObservedGeneration(*decision.ObservedGeneration)
	}

	if decision.ForceReconcileTime != nil {
		rel.SetLastForceReconcileTime(*decision.ForceReconcileTime)
	}

	if decision.ApplyChart {
		rel.SetLastAppliedChart(rel.ChartRef())
	}

	if decision.ApplyValues {
		if rel.Values() == nil {
			rel.SetLastAppliedValues(nil)
		} else {
			rel.SetLastAppliedValues(rel.Values().DeepCopy())
		}
	}
}

// repositoryChangeFailure reports a failure to remove the internal source the
// release no longer needs. Nothing may be installed while both kinds are present.
func repositoryChangeFailure(err error) Failure {
	return Failure{Reason: helmv1alpha1.ReasonFailed, Message: "Repository change failed", Err: err}
}

// valuesDigest is the digest of the values the spec asks for, in the form the
// deployed revision records the values it was installed with.
func valuesDigest(rel contract.Release) string {
	rawValues := []byte(`{}`)
	if rel.Values() != nil {
		rawValues = rel.Values().Raw
	}

	values, _ := helmcommon.ReadValues(rawValues)

	return chartutil.DigestValues(digest.Canonical, values).String()
}

func (r *Reconciler) reconcileDelete(ctx context.Context, rel contract.Release) (reconcile.Result, error) {
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
	release, err := r.deps.Releases.Cleanup(ctx, names)
	if err != nil {
		return reconcile.Result{}, err
	}
	if release != nil {
		// The release object is a facade over the HelmRelease: a bad spec parameter
		// that blocks helm uninstall is propagated into the release. Keep re-applying
		// the (possibly corrected) spec to the still-present release so the
		// uninstall can be fixed via the release object even while it is being deleted.
		if err := r.deps.Releases.SyncSpec(ctx, rel, release); err != nil {
			return reconcile.Result{}, err
		}
		return pass.AwaitInternalResourceDeletion(ctx, r.deps.Status.MarkUninstallPending, rel.Object(), "internal release", release)
	}

	chart, err := r.deps.Charts.Cleanup(ctx, names)
	if err != nil {
		return reconcile.Result{}, err
	}
	if chart != nil {
		return pass.AwaitInternalResourceDeletion(ctx, r.deps.Status.MarkUninstallPending, rel.Object(), "internal chart", chart)
	}

	ociRepo, err := r.deps.OCIRepositories.Remove(ctx, names)
	if err != nil {
		return reconcile.Result{}, err
	}
	if ociRepo != nil {
		return pass.AwaitInternalResourceDeletion(ctx, r.deps.Status.MarkUninstallPending, rel.Object(), "internal repository", ociRepo)
	}

	// The identity goes last: helm-controller uninstalls as that account, so it
	// has to outlive the HelmRelease.
	if err := r.deps.Identity.Cleanup(ctx, rel); err != nil {
		// By this point the internal release and sources are already gone, so
		// nothing else on the object would explain why the finalizer is still
		// there. The write is best-effort, same as the internal-resource waits
		// above: the returned error is what gets this retried.
		_ = r.deps.Status.MarkDeletionFailed(ctx, rel.Object(), "release identity", err)
		return reconcile.Result{}, fmt.Errorf("cleaning up release identity: %w", err)
	}

	// Release the claim only once every downstream resource is gone: releasing it
	// earlier would let another release start reconciling the same chart while this
	// one's HelmRelease is still being uninstalled — exactly the collision the claim
	// prevents.
	if err := r.deps.Claims.Drop(ctx, rel); err != nil {
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

// markMaintenanceChangeInProgress publishes Reconciling before the maintenance
// switch is attempted. Whoever edited spec.maintenance is watching the object, and
// between the request and the write on the internal HelmRelease the release is
// neither in maintenance nor out of it: the condition is what says the switch is
// being made. The pass that lands it takes the condition away again, and the one
// that fails to keeps it with the reason of the retry.
func (r *Reconciler) markMaintenanceChangeInProgress(ctx context.Context, rel contract.Release) error {
	err := r.deps.Status.PatchStatus(ctx, rel.Object(), func() {
		apimeta.SetStatusCondition(rel.Object().GetConditions(), metav1.Condition{
			Type:               helmv1alpha1.ConditionTypeReconciling,
			Status:             metav1.ConditionTrue,
			Reason:             helmv1alpha1.ReasonReconciling,
			Message:            "Changing maintenance mode",
			ObservedGeneration: rel.Generation(),
		})
	})
	if client.IgnoreNotFound(err) != nil {
		return fmt.Errorf("publishing maintenance change progress: %w", err)
	}

	return nil
}

// markForceReconcileInProgress publishes Reconciling before the work a force
// request asks for begins. A forced pass is the one case where someone is
// watching: they annotated the object a moment ago and want to see it was picked
// up. The condition is removed again by the status update that ends the pass.
func (r *Reconciler) markForceReconcileInProgress(ctx context.Context, rel contract.Release) error {
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
	catalog contract.Catalog,
	repo contract.Repository,
	rel contract.Release,
	repoType chartsource.Kind,
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

		if repoType == chartsource.OCI && version.OCIRef == "" && version.MediaType == "" {
			return nil, nil, fmt.Errorf(
				"chart version %q cannot be deployed: %s",
				version.Version, versionUnavailableDetail(*version),
			)
		}

		return obj, version, nil
	}

	return nil, nil, fmt.Errorf("chart catalog does not have version %q", ref.Version)
}

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
	rel contract.Release,
	kind chartsource.Kind,
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
