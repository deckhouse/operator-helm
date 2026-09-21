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
	"github.com/deckhouse/operator-helm/internal/reconcile/pass"
	"github.com/deckhouse/operator-helm/internal/source"
	"github.com/deckhouse/operator-helm/internal/status"
	"github.com/deckhouse/operator-helm/internal/utils"
)

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
	Repositories RepositoryResolver
	Chart        ChartManager
	OCI          OCIRepoManager
	Release      ReleaseManager
	Maintenance  MaintenanceManager
	Claim        ChartClaim
	Namespaces   TargetNamespaceEnsurer
	Access       AccessManager
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
	acquired, holder, err := r.deps.Claim.Acquire(ctx, rel)
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("acquiring chart claim: %w", err)
	}
	if !acquired {
		in.Step = &Failure{
			Reason: helmv1alpha1.ReasonChartClaimConflict,
			Message: fmt.Sprintf("chart %q is already used by %s/%s",
				rel.ChartRef().Chart, strings.ToLower(rel.Kind()), holder),
			// No watch fires when the owner lets the pair go, so the recovery of a
			// duplicate rides on this timer alone.
			RequeueAfter: chartClaimConflictRequeueInterval,
		}

		return r.finish(ctx, rel, in)
	}

	if utils.IsSystemNamespace(rel.TargetNamespace()) {
		in.Step = &Failure{
			Reason:   helmv1alpha1.ReasonFailed,
			Message:  "Target namespace cannot be a system namespace",
			Err:      fmt.Errorf("target namespace %q is a system namespace", rel.TargetNamespace()),
			Terminal: true,
		}

		return r.finish(ctx, rel, in)
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
		outcome := r.deps.Maintenance.EnsureMaintenanceMode(ctx, rel)
		in.Maintenance = &outcome
		// Maintenance being lifted leaves a pending force request in place for the
		// pass that can honour it; a release settling into maintenance can never act
		// on one, so it is dropped.
		in.DiscardForce = rel.MaintenanceActivated()

		return r.finish(ctx, rel, in)
	}

	if rel.MaintenanceActivated() {
		in.DiscardForce = true

		return r.finish(ctx, rel, in)
	}

	repo, catalog, err := r.deps.Repositories.Resolve(ctx, rel.ChartRef().Repository)
	if err != nil {
		in.Step = &Failure{
			Reason:  helmv1alpha1.ReasonFailed,
			Message: "Failed to get internal repository",
			Err:     fmt.Errorf("getting internal repository: %w", err),
		}

		return r.finish(ctx, rel, in)
	}

	repoType, err := chartsource.KindOf(repo.URL())
	if err != nil {
		// The repository's own url is what cannot be read, and the repository reports
		// the same fault as Stalled. Retrying here would only rediscover it; the
		// release comes back when the repository's generation changes, which is what
		// correcting the url does.
		in.Step = &Failure{
			Reason:   helmv1alpha1.ReasonUnsupportedRepositoryType,
			Message:  fmt.Sprintf("Failed to parse repository type: %s", err.Error()),
			Err:      err,
			Terminal: true,
		}

		return r.finish(ctx, rel, in)
	}

	if err := r.deps.Namespaces.EnsureTargetNamespace(ctx, rel); err != nil {
		in.Step = &Failure{
			Reason:  helmv1alpha1.ReasonFailed,
			Message: fmt.Sprintf("Failed to reconcile target namespace: %s", err.Error()),
			Err:     err,
		}

		return r.finish(ctx, rel, in)
	}

	// The identity comes before the internal sources: helm-controller checks that
	// the account named on the HelmRelease exists before it impersonates it, so a
	// HelmRelease created ahead of its ServiceAccount would fail its first pass.
	if access := r.deps.Access.EnsureAccess(ctx, rel); access.Err != nil {
		in.Step = &Failure{
			Reason:   access.Reason,
			Message:  access.Message,
			Err:      access.Err,
			Terminal: access.Terminal,
			// The watches on the Role and the RoleBinding only fire on a write that
			// landed, so a step that failed before writing anything comes back through
			// the work queue's rate limiter and nothing else. A terminal failure is the
			// exception: the object in the way carries no managed-by label, so those
			// watches never see it go either — only a force request or an edit to the
			// release gets this pass run again.
			Retry: !access.Terminal,
		}

		return r.finish(ctx, rel, in)
	}

	// From here on every path reaches finish, which is what consumes the force
	// request. Marking earlier would leave the progress condition behind on a
	// validation failure that never consumes it.
	in.Forced = rel.ForceReconcileRequired()
	if in.Forced {
		if err := r.markForceReconcileInProgress(ctx, rel); err != nil {
			return reconcile.Result{}, err
		}
	}

	_, chartVersion, chartErr := r.getChartVersion(ctx, catalog, repo, rel, repoType)

	// The source is resolved once, before the branches: which internal object a
	// release needs is a property of the version it asks for, and a version whose
	// source cannot be resolved is as unusable as a version that is missing.
	var src chartsource.Source
	if chartErr == nil {
		src, chartErr = chartsource.Resolve(repo.URL(), chartVersion)
	}

	names := rel.InternalNames()

	switch {
	case chartErr != nil:
		// One report for both branches: until the source is known, neither internal
		// object may be touched, and which one would have been touched is precisely
		// what could not be determined.
		in.ChartSource = &Failure{
			Reason:  helmv1alpha1.ReasonChartFetchFailed,
			Message: "Failed to resolve the desired chart version",
			Err:     chartErr,
		}
	case src.Kind == chartsource.Helm:
		// The version may have moved out of a registry — either because the user
		// repointed the repository, or because the index re-published it as an
		// archive. Either way the internal OCIRepository is no longer the source.
		superseded, err := r.deps.OCI.RemoveOCIRepository(ctx, names)
		if err != nil {
			in.ChartSource = repositoryChangeFailure(err)

			break
		}

		r.logSourceKindFlip(ctx, rel, src.Kind, superseded != nil)

		outcome := r.deps.Chart.EnsureHelmChart(ctx, rel, repo)
		in.Chart = &outcome
	case src.Kind == chartsource.OCI:
		superseded, err := r.deps.Chart.CleanupHelmChart(ctx, names)
		if err != nil {
			in.ChartSource = repositoryChangeFailure(err)

			break
		}

		r.logSourceKindFlip(ctx, rel, src.Kind, superseded != nil)

		outcome := r.deps.OCI.EnsureInternalOCIRepository(ctx, rel, repo, src, chartVersion)
		in.OCIRepo = &outcome
	default:
		in.Step = &Failure{
			Reason:   helmv1alpha1.ReasonFailed,
			Message:  fmt.Sprintf("Unsupported chart source: %s", src.Kind),
			Err:      fmt.Errorf("unsupported chart source: %s", src.Kind),
			Terminal: true,
		}

		return r.finish(ctx, rel, in)
	}

	if revision, ok := artifactRevision(in); ok {
		outcome := r.deps.Release.EnsureHelmRelease(ctx, rel, src.Kind, revision)
		in.Release = &outcome
	}

	in.ChartInfoOutdated = rel.IsChartStatusInfoOutdated()
	in.ValuesDigest = valuesDigest(rel)

	return r.finish(ctx, rel, in)
}

// finish applies the decision and consumes the force annotation. The annotation is
// removed after the status patch so a conflict does not lose the request.
func (r *Reconciler) finish(ctx context.Context, rel source.Release, in Inputs) (reconcile.Result, error) {
	decision := Evaluate(in)

	if decision.Reported != nil {
		// Only the access failure is handed to the work queue, which logs it on the
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
func applyDecision(rel source.Release, decision Decision) {
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
func repositoryChangeFailure(err error) *Failure {
	return &Failure{Reason: helmv1alpha1.ReasonFailed, Message: "Repository change failed", Err: err}
}

// valuesDigest is the digest of the values the spec asks for, in the form the
// deployed revision records the values it was installed with.
func valuesDigest(rel source.Release) string {
	rawValues := []byte(`{}`)
	if rel.Values() != nil {
		rawValues = rel.Values().Raw
	}

	values, _ := helmcommon.ReadValues(rawValues)

	return chartutil.DigestValues(digest.Canonical, values).String()
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
		return pass.AwaitInternalResourceDeletion(ctx, r.deps.Status.MarkUninstallPending, rel.Object(), "internal release", release)
	}

	chart, err := r.deps.Chart.CleanupHelmChart(ctx, names)
	if err != nil {
		return reconcile.Result{}, err
	}
	if chart != nil {
		return pass.AwaitInternalResourceDeletion(ctx, r.deps.Status.MarkUninstallPending, rel.Object(), "internal chart", chart)
	}

	ociRepo, err := r.deps.OCI.RemoveOCIRepository(ctx, names)
	if err != nil {
		return reconcile.Result{}, err
	}
	if ociRepo != nil {
		return pass.AwaitInternalResourceDeletion(ctx, r.deps.Status.MarkUninstallPending, rel.Object(), "internal repository", ociRepo)
	}

	// The identity goes last: helm-controller uninstalls as that account, so it
	// has to outlive the HelmRelease.
	if err := r.deps.Access.CleanupAccess(ctx, rel); err != nil {
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
