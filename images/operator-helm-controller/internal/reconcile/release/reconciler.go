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
	"errors"
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
	// Before claiming anything, let go of the pairs this release has moved off. A
	// claim is only ever released by the release that holds it, so this must not sit
	// behind a claim that may not be granted — a pass that fails to acquire still
	// releases. What it may release is narrower than "everything the spec no longer
	// names": see DropStale, which lets a pair go only once the move off it is on
	// the status.
	if err := r.deps.Claims.DropStale(ctx, rel); err != nil {
		return reconcile.Result{}, fmt.Errorf("releasing stale chart claims: %w", err)
	}

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
			// Nothing watches Namespaces, so a failure here rides the work queue — and a
			// namespace the API server will never accept would ride it forever, logging the
			// same rejection at every attempt and making no progress at all.
			Terminal: rejectedByAPIServer(err),
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

	switch {
	case errors.Is(err, errChartNotFound):
		// The name itself is wrong, which the version-level message would misreport as
		// a version that is merely missing.
		return src, chartVersion, &Failure{
			Reason: helmv1alpha1.ReasonChartVersionNotFound,
			Message: fmt.Sprintf("Chart %s is not in the catalog of repository %s",
				rel.ChartRef().Chart, repo.Name()),
			Err:      err,
			BadInput: true,
		}
	case errors.Is(err, errChartVersionNotFound):
		// The catalog is populated and the version the spec names is not in it, which
		// is what a typo looks like. It is reported as a fault of the spec rather than
		// as work in flight: no pass of this controller is on its way to that version.
		return src, chartVersion, &Failure{
			Reason: helmv1alpha1.ReasonChartVersionNotFound,
			Message: fmt.Sprintf("Version %s of chart %s is not in the catalog of repository %s",
				rel.ChartRef().Version, rel.ChartRef().Chart, repo.Name()),
			Err:      err,
			BadInput: true,
		}
	case errors.Is(err, errVersionUnusable):
		// The version is listed but the repository published it in a way this operator
		// cannot deploy from. Retrying resolves nothing — the catalog would have to
		// change, and its watches bring the release back when it does.
		return src, chartVersion, &Failure{
			Reason: helmv1alpha1.ReasonChartVersionNotFound,
			Message: fmt.Sprintf("Version %s of chart %s cannot be deployed from repository %s",
				rel.ChartRef().Version, rel.ChartRef().Chart, repo.Name()),
			Err:      err,
			BadInput: true,
		}
	case errors.Is(err, errVersionResolvePending):
		// The version is in the catalog and the repository has yet to reach a verdict
		// on it. Reaching one is a write to the catalog object the release watches,
		// but only a write that changes something: a registry that keeps failing the
		// same way leaves the entry byte for byte as it was, and no event follows. The
		// timer is what makes the release pick up the verdict when it does land.
		return src, chartVersion, &Failure{
			Reason:      helmv1alpha1.ReasonChartFetchFailed,
			Message:     "Failed to resolve the desired chart version",
			Err:         err,
			AwaitsWatch: true,
			RetryAfter:  awaitSyncInterval,
		}
	case errors.Is(err, errCatalogAwaited):
		// Waiting on the repository's first sync of its current spec, which nothing
		// wakes the release from: a mistyped chart name has no catalog object and so
		// no watch event at all, and the sync time that would settle the question is
		// written to the repository's status, which the release's watch on the
		// repository filters out by generation. Hence a timer rather than the work
		// queue — the wait is expected, and the queue would log it as a fault.
		return src, chartVersion, &Failure{
			Reason:      helmv1alpha1.ReasonChartFetchFailed,
			Message:     "Failed to resolve the desired chart version",
			Err:         err,
			AwaitsWatch: true,
			RetryAfter:  awaitSyncInterval,
		}
	case err != nil:
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
		// A failure handed to the work queue is logged again by controller-runtime on
		// the way past; one that is not ends the pass quietly, and this is then the one
		// place its cause is written down.
		//
		// Bad input is logged at info: the release names a chart version its repository
		// does not offer, which is answered in its status and is no fault of this
		// controller. Reporting it as a controller error would also fail the e2e suite,
		// which treats any error line as a defect.
		if decision.Reported.BadInput {
			log.FromContext(ctx).Info(decision.Reported.Message,
				"reason", decision.Reported.Reason, "cause", decision.Reported.Err)
		} else {
			log.FromContext(ctx).Error(decision.Reported.Err, decision.Reported.Message,
				"reason", decision.Reported.Reason)
		}
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
	//
	// A terminating target namespace is the one case that order cannot serve, and it
	// is handled before the first wait: see removeIdentityOfTerminatingNamespace.
	if err := r.removeIdentityOfTerminatingNamespace(ctx, rel); err != nil {
		return reconcile.Result{}, err
	}

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
	identity, err := r.deps.Identity.Cleanup(ctx, rel)
	if err != nil {
		// By this point the internal release and sources are already gone, so
		// nothing else on the object would explain why the finalizer is still
		// there. The write is best-effort, same as the internal-resource waits
		// above: the returned error is what gets this retried.
		_ = r.deps.Status.MarkDeletionFailed(ctx, rel.Object(), "release identity", err)
		return reconcile.Result{}, fmt.Errorf("cleaning up release identity: %w", err)
	}
	if identity != nil {
		// The identity objects carry no ownerReference — the account lives in another
		// namespace than the release, where a cross-namespace owner reference does not
		// work, and the binding is deleted by name here. Nothing would collect them if
		// this object went away while a finalizer of someone else's still held one of
		// them Terminating, and the next application of the namespace would then find
		// it under the same derived name and stall on it as foreign.
		return pass.AwaitInternalResourceDeletion(ctx, r.deps.Status.MarkDeletionPending, rel.Object(), "release identity", identity)
	}

	// Release the claim only once every downstream resource is gone: releasing it
	// earlier would let another release start reconciling the same chart while this
	// one's HelmRelease is still being uninstalled — exactly the collision the claim
	// prevents.
	// Every pair, not only the one the spec names: a release deleted after a chart
	// change still holds the one it moved off, and once this object is gone nobody
	// is left who may release it.
	if err := r.deps.Claims.DropAll(ctx, rel); err != nil {
		return reconcile.Result{}, fmt.Errorf("releasing chart claims: %w", err)
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

// removeIdentityOfTerminatingNamespace takes the release's ServiceAccount away
// before anything is waited on, when the namespace the release deploys into is
// terminating.
//
// The ordinary teardown deletes the identity last, because helm-controller
// uninstalls as that account and it has to outlive the internal HelmRelease. A
// namespace that is going away breaks that: its controller deletes the Role and the
// RoleBinding living there, while the account lives in the operator's namespace and
// survives, so the identity the HelmRelease impersonates exists and may do nothing.
// The uninstall then reads the Helm storage secret in the terminating namespace,
// is refused, and the wait on the internal HelmRelease below never ends. Upstream's
// way out only fires for an account that is absent, so the account is made absent.
//
// Nothing about the ordinary path changes: outside this branch the identity is
// still removed last, and here there is nothing left for it to outlive — every
// object the uninstall would have touched is being deleted with the namespace.
//
// The wait the release reports is unchanged too. The status of this teardown is the
// status of whatever it is still waiting on, written by the steps below; a failure
// to remove the account is reported the way every other deletion failure of a
// resource without conditions of its own is, and handed back so the work queue
// retries it.
func (r *Reconciler) removeIdentityOfTerminatingNamespace(ctx context.Context, rel contract.Release) error {
	if rel.InternalNames().ServiceAccount == "" {
		// A family that does not impersonate is applied as helm-controller itself, an
		// identity no namespace deletion can take apart. There is nothing to repair, and
		// no reason to read the namespace on every pass of every such teardown.
		return nil
	}

	terminating, err := r.deps.Namespaces.Terminating(ctx, rel)
	if err != nil {
		err = fmt.Errorf("checking whether the target namespace is terminating: %w", err)
		// Whether the account has to go early is unknown, so the teardown stops here
		// rather than taking the ordinary order on a guess. Nothing later in this pass
		// writes a status then, which is why one is written here.
		_ = r.deps.Status.MarkDeletionFailed(ctx, rel.Object(), "release identity", err)

		return err
	}
	if !terminating {
		return nil
	}

	if err := r.deps.Identity.RemoveAccount(ctx, rel); err != nil {
		err = fmt.Errorf("removing the release identity of a terminating namespace: %w", err)
		_ = r.deps.Status.MarkDeletionFailed(ctx, rel.Object(), "release identity", err)

		return err
	}

	log.FromContext(ctx).Info(
		"Target namespace is terminating; removing the release identity first so the uninstall is skipped",
		"namespace", rel.TargetNamespace(),
	)

	return nil
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

	// Whether the repository has finished a sync of the spec it currently carries is
	// what makes anything the catalog says final. Before that the catalog describes
	// some earlier spec, or nothing at all, and neither an absence nor a listing in
	// it answers about the repository the release points at now.
	synced := catalogIsCurrent(repo)

	obj, catalogStatus, err := catalog.Lookup(ctx, repo, ref.Chart)
	if err != nil {
		if apierrors.IsNotFound(err) {
			if !synced {
				// The repository controller writes the catalog object, and the release is
				// watching for it. Until it lands there is nothing here to retry against.
				return nil, nil, fmt.Errorf("getting chart catalog entry: %w: %w", err, errCatalogAwaited)
			}

			return nil, nil, fmt.Errorf(
				"chart %q is not in repository %q: %w", ref.Chart, repo.Name(), errChartNotFound,
			)
		}

		return nil, nil, fmt.Errorf("getting chart catalog entry: %w", err)
	}

	if !synced {
		// Even a populated catalog is the previous spec's answer until the sync of the
		// current one lands: a version this release asks for may be offered by the new
		// url and absent from the old catalog, and calling that a fault of the spec
		// would be wrong for as long as the resync takes.
		if !hasVersion(catalogStatus, ref.Version) {
			return nil, nil, fmt.Errorf(
				"chart catalog is not current: %w", errCatalogAwaited,
			)
		}
	}

	for i := range catalogStatus.Versions {
		version := &catalogStatus.Versions[i]
		if version.Version != ref.Version {
			continue
		}

		if version.UnavailableReason == helmv1alpha1.UnavailableReasonResolvePending &&
			version.OCIRef == "" && version.MediaType == "" {
			// The repository could not read the manifest and reached no verdict, so the
			// version may well be deployable once the next sync gets an answer. That is
			// a catalog write, which the release's catalog watch delivers.
			//
			// A pending verdict over a media type an earlier sync did resolve is a
			// different thing and is not caught here: a forced re-examination that fails
			// carries the known media type forward, the version is still deployable from
			// it, and no later sync will revisit the version precisely because it is
			// recorded. Blocking on that would hold the release — its values, its
			// maintenance mode — on a registry failure it has already outlived. The
			// OCIRef half of the test is for symmetry; the catalog never records a
			// reference for a version it left pending.
			return nil, nil, fmt.Errorf(
				"chart version %q is not resolved yet: %s: %w",
				version.Version, versionUnavailableDetail(*version), errVersionResolvePending,
			)
		}

		if version.UnavailableReason == helmv1alpha1.UnavailableReasonInvalidChartReference {
			// The index publishes this version in a registry at a reference that
			// cannot be addressed. Without this the version would fall back to the
			// helm path and fail on the very same url, reported by the source
			// controller as an opaque fetch error.
			//
			// Nothing this controller does changes it: the reference is what the
			// repository published, and it takes a republish — which the catalog
			// watches deliver — to make the version deployable.
			return nil, nil, fmt.Errorf(
				"chart version %q cannot be deployed: %s: %w",
				version.Version, versionUnavailableDetail(*version), errVersionUnusable,
			)
		}

		if repoType == chartsource.OCI && version.OCIRef == "" && version.MediaType == "" {
			// The catalog lists the version but knows nothing this operator can build a
			// source from. A verdict of its own — an unsupported media type, say — is
			// the repository's final word; no verdict at all is handled above.
			return nil, nil, fmt.Errorf(
				"chart version %q cannot be deployed: %s: %w",
				version.Version, versionUnavailableDetail(*version), errVersionUnusable,
			)
		}

		return obj, version, nil
	}

	if len(catalogStatus.Versions) == 0 {
		if !synced {
			// The catalog object is created before the versions are written onto it, so
			// an empty one before the first sync is a half-built catalog.
			return nil, nil, fmt.Errorf("chart catalog has no versions yet: %w", errCatalogAwaited)
		}

		// After a sync an empty catalog is durable, not transient: a chart whose every
		// entry the index marks removed, or publishes under a version this operator
		// cannot address, is recorded with no versions and stays that way until the
		// repository itself changes. Waiting on it forever would leave the release
		// claiming progress that nothing is making.
		return nil, nil, fmt.Errorf(
			"chart %q in repository %q has no usable versions: %w",
			ref.Chart, repo.Name(), errChartVersionNotFound,
		)
	}

	return nil, nil, fmt.Errorf("chart catalog does not have version %q: %w", ref.Version, errChartVersionNotFound)
}

// errChartVersionNotFound marks the one way of failing to resolve a version that is
// a fault of the release's own spec: the catalog lists what the repository offers
// and the version asked for is not among it. It is a sentinel rather than a plain
// message because the caller reports it differently from every other resolution
// failure — as a bad input, not as progress.
var errChartVersionNotFound = errors.New("chart version not found")

// errCatalogAwaited marks a resolution failure that the catalog watches undo by
// themselves: the catalog object of this chart is not there yet, or is there and
// still empty. Both are the repository controller being behind, and both end the
// moment it writes — which enqueues the release. Handing such a failure to the work
// queue as well would spend the rate limiter re-running a pass whose input has not
// moved, and log a controller error for a state that is merely early.
var errCatalogAwaited = errors.New("chart catalog not written yet")

// errChartNotFound is the chart-level counterpart of errChartVersionNotFound: the
// repository has answered in full and offers no chart under that name at all.
var errChartNotFound = errors.New("chart not found")

// catalogIsCurrent reports whether the repository's catalog describes the spec the
// repository carries now.
//
// The question is which spec the published catalog belongs to, and only
// lastSyncedGeneration answers it. The status-level observedGeneration is advanced
// by every pass including the ones that fail, so one failed fetch after a url edit
// would make the previous url's catalog look current. The Synced condition is the
// verdict of the latest pass rather than a record of what is published: a transient
// fetch failure, or a chart name the API cannot hold, turns it False over a catalog
// that is complete and unchanged, and reading that as "not current" would put every
// release on this repository back to waiting.
func catalogIsCurrent(repo contract.Repository) bool {
	return repo.Status().LastSyncedGeneration == repo.Generation()
}

// hasVersion reports whether the catalog lists a version under that exact name.
func hasVersion(catalogStatus *helmv1alpha1.ChartCatalogStatus, version string) bool {
	for i := range catalogStatus.Versions {
		if catalogStatus.Versions[i].Version == version {
			return true
		}
	}

	return false
}

// errVersionResolvePending marks a version the catalog lists without a verdict on
// how it is published. The repository reaches one by writing to the catalog object,
// which the release watches — but a repeated identical failure writes nothing, so
// the wait carries a timer rather than relying on the watch alone.
var errVersionResolvePending = errors.New("chart version not resolved yet")

// awaitSyncInterval is how often a release re-reads a repository it is waiting on
// the first sync of. It is a poll, so it is deliberately coarse: the case it covers
// lasts as long as one sync of one repository, and every other path back into a
// pass is event-driven.
const awaitSyncInterval = 30 * time.Second

// errVersionUnusable marks a version the catalog lists but cannot be deployed from,
// because of what the repository published rather than what the spec asked for. It
// is reported the same way as a missing version: the release says so and waits for
// the catalog to change, which its watches deliver.
var errVersionUnusable = errors.New("chart version cannot be deployed")

// rejectedByAPIServer reports whether an API error is the server refusing the
// object it was handed, rather than failing to deal with it right now. The two
// answers differ in whether a retry can change them: a conflict, a timeout or an
// unreachable API server is a passing fault and belongs in the work queue, while a
// request the server judged invalid comes back judged the same way forever.
//
// The target namespace is where this matters. Neither the CRD schema (length only)
// nor the webhook (system namespaces only) knows the DNS label rules the API server
// applies to a Namespace name, so a spec.namespace like "Team-A" reaches this
// controller and is rejected as Invalid on every single attempt. Forbidden is left
// out on purpose: it is the one refusal that is routinely repaired out of band, by
// granting the missing permission, and a release stalled on it would then need a
// force request nobody knows to make.
func rejectedByAPIServer(err error) bool {
	return apierrors.IsInvalid(err) || apierrors.IsBadRequest(err)
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
