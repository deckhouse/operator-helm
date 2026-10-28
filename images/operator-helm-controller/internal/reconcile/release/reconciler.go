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
	"github.com/deckhouse/operator-helm/internal/force"
	"github.com/deckhouse/operator-helm/internal/status"
	"github.com/deckhouse/operator-helm/internal/utils"
)

const (
	// chartClaimConflictRequeueInterval bounds how often a release that lost the claim on its
	// repository/chart pair re-checks whether the owner has released it. Nothing watches the Lease.
	chartClaimConflictRequeueInterval = 30 * time.Second

	// awaitSyncInterval is how often a release re-reads a repository it is waiting on the first
	// sync of. It is a poll, so it is deliberately coarse.
	awaitSyncInterval = 30 * time.Second
)

var (
	// errChartVersionNotFound marks the one way of failing to resolve a version that is a fault
	// of the release's own spec: the version asked for is not among what the catalog lists.
	errChartVersionNotFound = errors.New("chart version not found")

	// errCatalogAwaited marks a resolution failure that the catalog watches undo by themselves:
	// the catalog object of this chart is not there yet, or is there and still empty.
	errCatalogAwaited = errors.New("chart catalog not written yet")

	// errChartNotFound is the chart-level counterpart of errChartVersionNotFound: the
	// repository has answered in full and offers no chart under that name at all.
	errChartNotFound = errors.New("chart not found")

	// errVersionResolvePending marks a version the catalog lists without a verdict on how it is
	// published. A repeated identical failure writes nothing, so the wait carries a timer.
	errVersionResolvePending = errors.New("chart version not resolved yet")

	// errVersionUnusable marks a version the catalog lists but cannot be deployed from, because
	// of what the repository published rather than what the spec asked for.
	errVersionUnusable = errors.New("chart version cannot be deployed")
)

// Deps are the collaborators of one release kind. Their implementations are shared by every
// kind, and what they do is what tells the families apart.
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

	// Released before acquiring, so a pass that goes on to fail acquisition still frees what it left.
	if err := r.deps.Claims.DropStale(ctx, rel); err != nil {
		return reconcile.Result{}, fmt.Errorf("releasing stale chart claims: %w", err)
	}

	// Claimed before the finalizer, so a duplicate that loses the race accrues none to clean up.
	acquired, holder, err := r.deps.Claims.Acquire(ctx, rel)
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("acquiring chart claim: %w", err)
	}
	if !acquired {
		return r.applyDecision(ctx, rel, in, StepFailed{Failure{
			Reason: helmv1alpha1.ReasonChartClaimConflict,
			Message: fmt.Sprintf("chart %q is already used by %s/%s",
				rel.ChartRef().Chart, strings.ToLower(rel.Kind()), holder),
			// No watch fires when the owner lets the pair go; this timer is the only recovery.
			RequeueAfter: chartClaimConflictRequeueInterval,
		}})
	}

	if utils.IsSystemNamespace(rel.TargetNamespace()) {
		return r.applyDecision(ctx, rel, in, StepFailed{Failure{
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
		// Continue in the same pass: adding a finalizer bumps no generation, so no event follows.
	}

	if r.deps.Maintenance.ChangeRequired(rel) {
		if err := r.markMaintenanceChangeInProgress(ctx, rel); err != nil {
			return reconcile.Result{}, err
		}

		result := r.deps.Maintenance.Ensure(ctx, rel)
		if result.Err != nil || result.Activated {
			return r.applyDecision(ctx, rel, in, MaintenanceChanged{Result: result})
		}

		// Maintenance was lifted, so this pass has to go on and actually deploy the chart.
		in.MaintenanceLifted = true
	}

	if rel.MaintenanceActivated() {
		return r.applyDecision(ctx, rel, in, InMaintenance{})
	}

	repo, catalog, err := r.deps.Repositories.Resolve(ctx, rel.ChartRef().Repository)
	if err != nil {
		return r.applyDecision(ctx, rel, in, StepFailed{Failure{
			Reason:  helmv1alpha1.ReasonFailed,
			Message: "Failed to get internal repository",
			Err:     fmt.Errorf("getting internal repository: %w", err),
			// The repository kinds are watched, so only a missing one has an event to wake on.
			AwaitsWatch: apierrors.IsNotFound(err),
		}})
	}

	repoType, err := chartsource.KindOf(repo.URL())
	if err != nil {
		// The fault is the repository's url; the release comes back when its generation changes.
		return r.applyDecision(ctx, rel, in, StepFailed{Failure{
			Reason:   helmv1alpha1.ReasonUnsupportedRepositoryType,
			Message:  fmt.Sprintf("Failed to parse repository type: %s", err.Error()),
			Err:      err,
			Terminal: true,
		}})
	}

	if err := r.deps.Namespaces.EnsureTarget(ctx, rel); err != nil {
		return r.applyDecision(ctx, rel, in, StepFailed{Failure{
			Reason:  helmv1alpha1.ReasonFailed,
			Message: fmt.Sprintf("Failed to reconcile target namespace: %s", err.Error()),
			Err:     err,
			// Nothing watches Namespaces, so a name the API server always rejects would spin forever.
			Terminal: rejectedByAPIServer(err),
		}})
	}

	// Before the internal sources: helm-controller checks the account exists before impersonating.
	if identity := r.deps.Identity.Ensure(ctx, rel); identity.Err != nil {
		return r.applyDecision(ctx, rel, in, StepFailed{Failure{
			Reason:   identity.Reason,
			Message:  identity.Message,
			Err:      identity.Err,
			Terminal: identity.Terminal,
		}})
	}

	// From here on every path reaches applyDecision, which is what consumes the force request.
	forced := rel.ForceReconcileRequired()
	if forced {
		if err := r.markForceReconcileInProgress(ctx, rel); err != nil {
			return reconcile.Result{}, err
		}
	}

	return r.applyDecision(ctx, rel, in, r.reconcileChart(ctx, rel, repo, catalog, repoType, forced))
}

// reconcileChart carries the pass to the internal objects: the source the version is served
// from and, once that source has an artifact, the internal HelmRelease.
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

	if revision, current, ok := artifactRevision(outcome.Internal); ok {
		result := r.deps.Releases.Ensure(ctx, rel, src.Kind, revision, current)
		outcome.Release = &result
	} else if inventory := r.deps.Releases.Inventory(ctx, rel.InternalNames()); inventory != nil {
		outcome.Inventory = &inventory
	}

	return outcome
}

// resolveSource is the version the release asks for and the source it is installed from. One
// report covers both, since until the source is known no internal object may be touched.
func (r *Reconciler) resolveSource(
	ctx context.Context,
	rel contract.Release,
	repo contract.Repository,
	catalog contract.Catalog,
	repoType chartsource.Kind,
) (chartsource.Source, *helmv1alpha1.ChartVersion, *Failure) {
	chartVersion, err := r.getChartVersion(ctx, catalog, repo, rel, repoType)

	var src chartsource.Source
	if err == nil {
		src, err = chartsource.Resolve(repo.URL(), chartVersion)
	}

	switch {
	case errors.Is(err, errChartNotFound):
		// The name itself is wrong; a version-level message would misreport that.
		return src, chartVersion, &Failure{
			Reason: helmv1alpha1.ReasonChartNotFound,
			Message: fmt.Sprintf("Chart %s is not in the catalog of repository %s",
				rel.ChartRef().Chart, repo.Name()),
			Err:      err,
			BadInput: true,
		}
	case errors.Is(err, errChartVersionNotFound):
		// Reported as a fault of the spec: no pass of this controller is on its way to it.
		return src, chartVersion, &Failure{
			Reason: helmv1alpha1.ReasonChartVersionNotFound,
			Message: fmt.Sprintf("Version %s of chart %s is not in the catalog of repository %s",
				rel.ChartRef().Version, rel.ChartRef().Chart, repo.Name()),
			Err:      err,
			BadInput: true,
		}
	case errors.Is(err, errVersionUnusable):
		// Retrying resolves nothing: only a catalog change can, and its watches bring us back.
		return src, chartVersion, &Failure{
			Reason: helmv1alpha1.ReasonChartVersionUnusable,
			Message: fmt.Sprintf("Version %s of chart %s cannot be deployed from repository %s",
				rel.ChartRef().Version, rel.ChartRef().Chart, repo.Name()),
			Err:      err,
			BadInput: true,
		}
	case errors.Is(err, errVersionResolvePending), errors.Is(err, errCatalogAwaited):
		// A registry that keeps failing the same way rewrites nothing, so no watch event follows,
		// and the sync time that settles a catalog is a status write the repository watch filters out.
		return src, chartVersion, &Failure{
			Reason:       helmv1alpha1.ReasonChartFetchFailed,
			Message:      "Failed to resolve the desired chart version",
			Err:          err,
			AwaitsWatch:  true,
			RequeueAfter: awaitSyncInterval,
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

// serveFromHelmChart takes the internal OCIRepository away before bringing up the internal
// HelmChart, since the version may have moved out of a registry.
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

// applyDecision applies the decision and consumes the force annotation. The annotation is
// removed after the status patch so a conflict does not lose the request.
func (r *Reconciler) applyDecision(
	ctx context.Context,
	rel contract.Release,
	in Inputs,
	outcome Outcome,
) (reconcile.Result, error) {
	in.Outcome = outcome
	decision := Evaluate(in)

	if decision.Reported != nil {
		// Bad input, a wait and a failure with no cause are logged at info: none is a fault of
		// this controller.
		if decision.Reported.Err == nil || decision.Reported.BadInput || decision.Reported.AwaitsWatch {
			kv := []any{"reason", decision.Reported.Reason}
			if decision.Reported.Err != nil {
				kv = append(kv, "cause", decision.Reported.Err)
			}

			log.FromContext(ctx).Info(decision.Reported.Message, kv...)
		} else {
			log.FromContext(ctx).Error(decision.Reported.Err, decision.Reported.Message,
				"reason", decision.Reported.Reason)
		}
	}

	if err := r.deps.Status.PatchStatus(ctx, rel.Object(), func() {
		writeStatus(rel, decision)
	}); client.IgnoreNotFound(err) != nil {
		return reconcile.Result{}, fmt.Errorf("failed to update status: %w", err)
	}

	if decision.ConsumeForce {
		if err := force.RemoveAnnotation(ctx, r.Client, client.ObjectKeyFromObject(rel.Object()), r.deps.NewRelease().Object()); err != nil {
			return reconcile.Result{}, fmt.Errorf("failed to reconcile force annotation: %w", err)
		}
	}

	return reconcile.Result{RequeueAfter: decision.RequeueAfter}, decision.Err
}

// writeStatus writes the evaluated status onto the release. Conditions are merged rather
// than replaced, so the verdicts of steps this pass did not run stay where they are.
func writeStatus(rel contract.Release, decision Decision) {
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

	if decision.Inventory != nil {
		rel.SetInventory(*decision.Inventory)
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

// repositoryChangeFailure reports a failure to remove the internal source the release no
// longer needs. Nothing may be installed while both kinds are present.
func repositoryChangeFailure(err error) Failure {
	return Failure{Reason: helmv1alpha1.ReasonFailed, Message: "Repository change failed", Err: err}
}

// valuesDigest is the digest of the values the spec asks for, in the form the deployed
// revision records the values it was installed with.
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

	// A terminating namespace is the one case that order cannot serve, so it is handled first.
	identityRemoved, err := r.removeIdentityOfTerminatingNamespace(ctx, rel)
	if err != nil {
		return reconcile.Result{}, err
	}

	// Torn down in order - release, then sources, then identity - each waited on before the next.
	release, err := r.deps.Releases.Cleanup(ctx, names)
	if err != nil {
		return reconcile.Result{}, err
	}
	if release != nil {
		// Re-applied so an uninstall blocked by a bad parameter can be fixed while deleting.
		if err := r.deps.Releases.SyncSpec(ctx, rel, release); err != nil {
			return reconcile.Result{}, err
		}

		// Repaired only while the release is still there: helm-controller uninstalls as it.
		if !identityRemoved {
			if identity := r.deps.Identity.Ensure(ctx, rel); identity.Err != nil {
				// Best-effort: what gets the pass retried is the error below, or the timer.
				_ = r.deps.Status.MarkDeletionFailed(ctx, rel.Object(), "release identity", identity.Err)

				if !identity.Terminal {
					return reconcile.Result{}, fmt.Errorf("repairing release identity: %w", identity.Err)
				}

				// No error is handed back, so this is the only place a foreign object is logged.
				logger.Error(identity.Err, identity.Message, "reason", identity.Reason)

				return reconcile.Result{RequeueAfter: status.DeletionRequeueInterval}, nil
			}
		}

		return status.AwaitDeletion(ctx, r.deps.Status.MarkUninstallPending, rel.Object(), "internal release", release)
	}

	chart, err := r.deps.Charts.Cleanup(ctx, names)
	if err != nil {
		return reconcile.Result{}, err
	}
	if chart != nil {
		return status.AwaitDeletion(ctx, r.deps.Status.MarkUninstallPending, rel.Object(), "internal chart", chart)
	}

	ociRepo, err := r.deps.OCIRepositories.Remove(ctx, names)
	if err != nil {
		return reconcile.Result{}, err
	}
	if ociRepo != nil {
		return status.AwaitDeletion(ctx, r.deps.Status.MarkUninstallPending, rel.Object(), "internal repository", ociRepo)
	}

	// The identity goes last: helm-controller uninstalls as that account.
	identity, err := r.deps.Identity.Cleanup(ctx, rel)
	if err != nil {
		// Best-effort: the returned error is what gets this retried.
		_ = r.deps.Status.MarkDeletionFailed(ctx, rel.Object(), "release identity", err)
		return reconcile.Result{}, fmt.Errorf("cleaning up release identity: %w", err)
	}
	if identity != nil {
		// The identity objects carry no ownerReference, so nothing would collect one left behind.
		return status.AwaitDeletion(ctx, r.deps.Status.MarkDeletionPending, rel.Object(), "release identity", identity)
	}

	// Released only once every downstream resource is gone, and every pair, not just the current.
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

// removeIdentityOfTerminatingNamespace takes the release's ServiceAccount away before anything
// is waited on, when the target namespace is terminating, and reports whether it did.
func (r *Reconciler) removeIdentityOfTerminatingNamespace(ctx context.Context, rel contract.Release) (bool, error) {
	// A family that does not impersonate has an identity no namespace deletion can take apart.
	if rel.InternalNames().ServiceAccount == "" {
		return false, nil
	}

	terminating, err := r.deps.Namespaces.Terminating(ctx, rel)
	if err != nil {
		err = fmt.Errorf("checking whether the target namespace is terminating: %w", err)
		// Nothing later in this pass writes a status, so one is written here.
		_ = r.deps.Status.MarkDeletionFailed(ctx, rel.Object(), "release identity", err)

		return false, err
	}
	if !terminating {
		return false, nil
	}

	if err := r.deps.Identity.RemoveAccount(ctx, rel); err != nil {
		err = fmt.Errorf("removing the release identity of a terminating namespace: %w", err)
		_ = r.deps.Status.MarkDeletionFailed(ctx, rel.Object(), "release identity", err)

		return false, err
	}

	log.FromContext(ctx).Info(
		"Target namespace is terminating; removing the release identity first so the uninstall is skipped",
		"namespace", rel.TargetNamespace(),
	)

	return true, nil
}

// markMaintenanceChangeInProgress publishes Reconciling before the maintenance switch is
// attempted. The pass that lands the switch takes the condition away again.
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

// markForceReconcileInProgress publishes Reconciling before the work a force request asks
// for begins. The status update that ends the pass removes the condition again.
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

// getChartVersion resolves the catalog entry for the version the release asks for and rejects
// an entry that cannot be deployed: an unaddressable reference, or a missing oci media type.
func (r *Reconciler) getChartVersion(
	ctx context.Context,
	catalog contract.Catalog,
	repo contract.Repository,
	rel contract.Release,
	repoType chartsource.Kind,
) (*helmv1alpha1.ChartVersion, error) {
	ref := rel.ChartRef()

	// Only a catalog synced for the current spec answers about the repository pointed at now.
	synced := catalogIsCurrent(repo)
	complete := catalogIsComplete(repo)

	_, catalogStatus, err := catalog.Lookup(ctx, repo, ref.Chart)
	if err != nil {
		if apierrors.IsNotFound(err) {
			if !synced {
				// The release watches for the catalog object; until it lands there is nothing to retry.
				return nil, fmt.Errorf("getting chart catalog entry: %w: %w", err, errCatalogAwaited)
			}

			return nil, fmt.Errorf(
				"chart %q is not in repository %q: %w", ref.Chart, repo.Name(), errChartNotFound,
			)
		}

		return nil, fmt.Errorf("getting chart catalog entry: %w", err)
	}

	if !synced {
		// Even a populated catalog is the previous spec's answer until the current sync lands.
		if !hasVersion(catalogStatus, ref.Version) {
			return nil, fmt.Errorf(
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
			// Only a version with nothing recorded: one that kept a resolved media type stays usable.
			return nil, fmt.Errorf(
				"chart version %q is not resolved yet: %s: %w",
				version.Version, versionUnavailableDetail(*version), errVersionResolvePending,
			)
		}

		if version.UnavailableReason == helmv1alpha1.UnavailableReasonInvalidChartReference {
			// Without this the version falls back to the helm path and fails on the same url.
			return nil, fmt.Errorf(
				"chart version %q cannot be deployed: %s: %w",
				version.Version, versionUnavailableDetail(*version), errVersionUnusable,
			)
		}

		if repoType == chartsource.OCI && version.OCIRef == "" && version.MediaType == "" {
			// The catalog lists the version but knows nothing a source can be built from.
			return nil, fmt.Errorf(
				"chart version %q cannot be deployed: %s: %w",
				version.Version, versionUnavailableDetail(*version), errVersionUnusable,
			)
		}

		return version, nil
	}

	if len(catalogStatus.Versions) == 0 {
		if !synced {
			// The catalog object is created before the versions are written onto it.
			return nil, fmt.Errorf("chart catalog has no versions yet: %w", errCatalogAwaited)
		}

		if !complete {
			return nil, fmt.Errorf(
				"chart catalog is incomplete and lists no version yet: %w", errCatalogAwaited,
			)
		}

		// After a sync an empty catalog is durable, not transient.
		return nil, fmt.Errorf(
			"chart %q in repository %q has no usable versions: %w",
			ref.Chart, repo.Name(), errChartVersionNotFound,
		)
	}

	if !complete {
		// Reporting a fault of the spec would be a guess about a version nobody has examined.
		return nil, fmt.Errorf(
			"chart catalog does not have version %q and is incomplete: %w",
			ref.Version, errCatalogAwaited,
		)
	}

	return nil, fmt.Errorf("chart catalog does not have version %q: %w", ref.Version, errChartVersionNotFound)
}

// catalogIsCurrent reports whether the repository's catalog describes the spec the repository
// carries now. Only lastSyncedGeneration answers that; observedGeneration and Synced do not.
func catalogIsCurrent(repo contract.Repository) bool {
	return repo.Status().LastSyncedGeneration == repo.Generation()
}

// catalogIsComplete reports whether the published catalog lists every version the repository
// offers. Only an unexamined version makes it incomplete.
func catalogIsComplete(repo contract.Repository) bool {
	return repo.Status().UnexaminedVersions == 0
}

func hasVersion(catalogStatus *helmv1alpha1.ChartCatalogStatus, version string) bool {
	for i := range catalogStatus.Versions {
		if catalogStatus.Versions[i].Version == version {
			return true
		}
	}

	return false
}

// rejectedByAPIServer reports whether an API error is the server refusing the object it was
// handed. Forbidden is left out: it is routinely repaired out of band.
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

// logSourceKindFlip reports that the repository index moved a chart version between an archive
// and a registry. superseded says whether a source of the other kind was removed in this pass.
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
		// The whole ref has to match: LastAppliedChart can lag behind a repointed spec.
		return
	}

	log.FromContext(ctx).Info(
		"Chart version changed where it is published; the running release will be upgraded from the new source",
		"version", rel.ChartRef().Version,
		"source", kind,
	)
}
