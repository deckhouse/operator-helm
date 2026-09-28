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

package catalog

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
	repoclient "github.com/deckhouse/operator-helm/internal/client/repository"
	"github.com/deckhouse/operator-helm/internal/contract"
)

// Config describes one chart catalog kind. C is the pointer type of the object,
// CL the pointer type of its list.
type Config[C client.Object, CL client.ObjectList] struct {
	// Kind names the catalog kind in log lines.
	Kind      string
	NewObject func() C
	NewList   func() CL
	// Items returns pointers into the list, so a status write through them lands
	// on the listed object.
	Items  func(CL) []C
	Status func(C) *helmv1alpha1.ChartCatalogStatus
	// ObjectName derives the catalog object name of one repository/chart pair; it
	// is one of the entries of api/naming.
	ObjectName func(repoName, chartName string) string
	// Consumers reports the versions of one chart that resources of the family
	// still reference. nil means nothing in the family can reference a chart yet,
	// and every unlisted version is prunable.
	Consumers func(ctx context.Context, repo contract.Repository, chartName string) (map[string]struct{}, error)
}

// New builds the contract.Catalog of one kind. reader reads catalog objects straight
// from the API server rather than from the manager's cache: everything a
// synchronization pass decides is computed from one such read (Snapshot), and a cached
// one can still show the state before this controller's own previous pass wrote it.
// The release path (Lookup) stays on the cached client, where a read that lags by an
// informer's latency only delays a resolution and cannot overwrite anything.
func New[C client.Object, CL client.ObjectList](c client.Client, reader client.Reader, cfg Config[C, CL]) contract.Catalog {
	return &typed[C, CL]{client: c, reader: reader, cfg: cfg}
}

type typed[C client.Object, CL client.ObjectList] struct {
	client client.Client
	reader client.Reader
	cfg    Config[C, CL]
}

// list returns the catalog objects of one repository, read through the API server. A
// namespaced repository's objects live in its namespace; for a cluster repository the
// namespace is empty and InNamespace is a no-op. Listing by namespace is what keeps two
// same-named repositories in different namespaces apart.
func (t *typed[C, CL]) list(ctx context.Context, repo contract.Repository) ([]C, error) {
	list := t.cfg.NewList()
	if err := t.reader.List(ctx, list,
		client.InNamespace(repo.Namespace()),
		client.MatchingLabels{helmv1alpha1.LabelRepositoryName: repo.Name()},
	); err != nil {
		return nil, fmt.Errorf("listing %s objects of repository %s: %w",
			t.cfg.Kind, describeKey(client.ObjectKey{Namespace: repo.Namespace(), Name: repo.Name()}), err)
	}

	return t.cfg.Items(list), nil
}

// Snapshot reads every catalog object of one repository and hands back the state the
// whole pass decides from. One read is the point of it: the verdicts the fetch skips
// tags by, the marks a forced refresh writes and the base each status patch is computed
// against all come from these objects, so no two inputs of the pass can describe the
// catalog differently. They are read through the API server rather than the manager's
// cache, because the previous pass wrote this very catalog and an informer that has not
// caught up would hand this one a state that has already been superseded.
//
// The read is taken once and used across the fetch, which can take the whole budget of
// a pass. What it cannot see is another writer in the meantime, and that is exactly
// what the optimistic lock on every write below is for.
func (t *typed[C, CL]) Snapshot(ctx context.Context, repo contract.Repository) (contract.CatalogSnapshot, error) {
	charts, err := t.list(ctx, repo)
	if err != nil {
		return nil, err
	}

	logger := log.FromContext(ctx)
	for _, chart := range charts {
		if chart.GetLabels()[helmv1alpha1.LabelChartName] == "" {
			// The chart label is the only way back from the object name (a
			// truncated hash) to the chart name it belongs to. Without it the
			// recorded verdicts for this chart cannot be looked up by the fetch, so
			// every tag is re-examined on the next one; that is safe but not free, so
			// it is worth surfacing.
			logger.Info("Chart object has no chart label, dropping its recorded verdicts",
				"kind", t.cfg.Kind, "chartObject", client.ObjectKeyFromObject(chart))
		}
	}

	return &snapshot[C, CL]{typed: t, repo: repo, charts: charts}, nil
}

// snapshot is one read of a repository's catalog objects. Its methods mutate those
// objects in place and patch them against themselves, so a write this pass made is part
// of every decision the rest of the pass takes without any read having to catch up.
type snapshot[C client.Object, CL client.ObjectList] struct {
	*typed[C, CL]

	repo   contract.Repository
	charts []C
}

// Known collects the verdicts recorded by previous passes, so the client can skip
// the tags it has already examined. The chart objects are the only store of that
// state: keeping a separate fingerprint would be one more thing to drift.
//
// It is computed from the snapshot each time it is asked for rather than once at the
// read, so a mark this pass wrote (MarkForReexamination) is in it: that mark is what
// makes the version a probe candidate again, and a verdict answered from before it
// would let the pass skip the very tag the refresh was asked for.
func (s *snapshot[C, CL]) Known() repoclient.KnownCharts {
	known := make(repoclient.KnownCharts, len(s.charts))

	for _, chart := range s.charts {
		chartName := chart.GetLabels()[helmv1alpha1.LabelChartName]
		if chartName == "" {
			// Reported by the read that built this snapshot; here it only means the
			// verdicts cannot be keyed under any chart name.
			continue
		}

		recorded := s.cfg.Status(chart).Versions
		versions := make(repoclient.KnownVersions, len(recorded))
		for _, version := range recorded {
			versions[version.Version] = repoclient.KnownVersion{
				MediaType:             version.MediaType,
				UnavailableReason:     version.UnavailableReason,
				UnavailableMessage:    version.UnavailableMessage,
				ReexaminationRequired: version.ReexaminationRequired,
			}
		}

		known[chartName] = versions
	}

	return known
}

// MarkForReexamination marks every verdict the catalog holds for the repository as
// owing a fresh one. It is how a forced refresh survives the pass that triggered it:
// the annotation is consumed by that pass, so unless the request is written down
// first, a read that never reaches the registry — or a catalog write that fails —
// leaves nothing behind, every later pass skips the versions whose media type is
// recorded, and the re-pushed tag the refresh was asked for is never examined again.
//
// The mark is not a verdict: an entry keeps its media type and stays deployable while
// it carries one, so nothing a release resolves changes between the mark and the pass
// that answers it.
//
// A version the repository no longer offers is marked like any other. It is retained
// only because a consumer references it, but its recorded verdict is exactly as stale
// as the rest: a tag that was deleted and pushed again is the re-pushed tag a forced
// refresh exists for, and leaving it unmarked would let an ordinary pass carry the old
// media type forward the moment the registry lists it again. The mark is not a dead end
// either — a pass that sees the whole repository and still does not list the version
// clears it while reporting the version removed (mergeChartVersions) — and it costs no
// request while the version stays unlisted, because the probe candidates are built from
// the registry listing alone.
//
// A version that is already awaiting a verdict (ResolvePending) is left alone: every
// pass re-examines it anyway, so the mark would buy nothing and would move the tag out
// of the group the probe order deliberately asks last.
//
// Objects the snapshot already found marked everywhere are not patched, so forcing a
// refresh on an unreachable registry twice in a row costs one write, not two. That the
// snapshot is an authoritative read is what makes the skip safe: deciding "already
// marked" from a cached object could skip the write while the catalog on the server
// carries no mark at all, and the force being consumed regardless, the refresh would be
// gone with nothing recording that it was ever asked for.
func (s *snapshot[C, CL]) MarkForReexamination(ctx context.Context) error {
	for _, chart := range s.charts {
		base := chart.DeepCopyObject().(C)
		status := s.cfg.Status(chart)

		patch := false

		for i := range status.Versions {
			if status.Versions[i].UnavailableReason == helmv1alpha1.UnavailableReasonResolvePending {
				continue
			}

			if !status.Versions[i].ReexaminationRequired {
				status.Versions[i].ReexaminationRequired = true
				patch = true
			}
		}

		if !patch {
			continue
		}

		if err := s.client.Status().Patch(ctx, chart, lockedTo(base)); err != nil {
			return fmt.Errorf("marking the versions of chart %s for re-examination: %w",
				describeKey(client.ObjectKeyFromObject(chart)), err)
		}
	}

	return nil
}

func (s *snapshot[C, CL]) Reconcile(ctx context.Context, charts []repoclient.Chart) (contract.CatalogResult, error) {
	logger := log.FromContext(ctx)

	var result contract.CatalogResult

	recorded := make(map[string]C, len(s.charts))
	for _, chart := range s.charts {
		recorded[chart.GetName()] = chart
	}

	desired := make(map[string]struct{}, len(charts))
	// A fetch that ran out of its budget describes the repository only in part, so
	// "the remote did not list it" — the sole reason anything is deleted below — is not
	// a conclusion this pass is entitled to draw about anything. Deleting a chart that
	// exists remotely costs its consumers their reconciliation, and the catalog is
	// rebuilt from a listing every few minutes anyway, so the pruning waits for a pass
	// that saw the whole repository.
	incomplete := false

	for _, chart := range charts {
		if len(chart.UnexaminedVersions) > 0 {
			incomplete = true
		}

		if errs := validation.IsValidLabelValue(chart.Name); len(errs) > 0 {
			// The chart label carries the name verbatim, and it is a query surface:
			// a user finds a catalog object by it. Writing a sanitized value would
			// answer the wrong query, and omitting the label would hide the object
			// from every such query while leaving it in the catalog. Neither is
			// better than not offering a chart whose name the API cannot hold — and
			// an index may legally publish one, so this must not cost the pass.
			//
			// The skip is reported to the caller because it is otherwise invisible: the
			// repository status would count a chart that has no object. The log line is
			// only the detail behind that report — which rule the name broke — and it is
			// logged at V(1) because the verdict never changes while the repository keeps
			// offering the name, and the catalog is re-read every few minutes.
			logger.V(1).Info("Skipping a chart whose name cannot be recorded on a catalog object",
				"kind", s.cfg.Kind, "chart", chart.Name, "reason", strings.Join(errs, "; "))

			result.SkippedCharts = append(result.SkippedCharts, chart.Name)

			continue
		}

		name := s.cfg.ObjectName(s.repo.Name(), chart.Name)
		desired[name] = struct{}{}

		// A chart with no usable version is still published: it carries the reason each
		// of its versions is unusable, and skipping it here would let the pruning loop
		// below delete a chart whose tags merely failed to resolve.
		known, present := recorded[name]

		existing, err := s.ensure(ctx, known, present, name, chart.Name)
		if err != nil {
			return result, err
		}

		inUse, err := s.InUseVersions(ctx, s.repo, chart.Name)
		if err != nil {
			return result, err
		}

		base := existing.DeepCopyObject().(C)

		status := s.cfg.Status(existing)
		if len(chart.Versions) > 0 {
			status.IconURL = chart.Versions[0].IconURL
		}
		status.Versions = mergeChartVersions(chart.Versions, status.Versions, inUse, chart.UnexaminedVersions)

		if err := s.client.Status().Patch(ctx, existing, lockedTo(base)); err != nil {
			return result, fmt.Errorf("updating versions of chart %s: %w", describeKey(client.ObjectKeyFromObject(existing)), err)
		}
	}

	if incomplete {
		logger.V(1).Info("Keeping every chart: the repository read was incomplete",
			"kind", s.cfg.Kind, "repository", describeKey(client.ObjectKey{Namespace: s.repo.Namespace(), Name: s.repo.Name()}))

		return result, nil
	}

	// Pruned against the same read the rest of the pass used: an object created after
	// it is one this pass knows nothing about, and the listing it would be judged by is
	// just as old, so leaving it to the next pass is the only honest answer.
	for _, chart := range s.charts {
		if _, wanted := desired[chart.GetName()]; wanted {
			continue
		}

		chartName := chart.GetLabels()[helmv1alpha1.LabelChartName]
		if chartName == "" {
			// The chart label is the only way back from the object name (a
			// truncated hash) to the chart name a consumer references, so
			// InUseVersions cannot find anything to protect and this chart is
			// pruned even if a consumer still uses it. That fail-open is unavoidable
			// as written, so at least make it diagnosable.
			logger.Info("Pruning a chart with no chart label; in-use protection could not be checked",
				"kind", s.cfg.Kind, "chartObject", client.ObjectKeyFromObject(chart))
		}

		inUse, err := s.InUseVersions(ctx, s.repo, chartName)
		if err != nil {
			return result, err
		}
		if len(inUse) > 0 {
			// A consumer still references this chart: deleting the object would make
			// its own reconciliation fail on a missing chart and block every change
			// to it, including its removal.
			logger.Info("Keeping a chart referenced by a consumer",
				"kind", s.cfg.Kind, "chartObject", client.ObjectKeyFromObject(chart))

			if err := s.recordAbsence(ctx, chart, inUse); err != nil {
				return result, err
			}

			continue
		}

		if err := client.IgnoreNotFound(s.client.Delete(ctx, chart)); err != nil {
			return result, fmt.Errorf("deleting stale charts: %w", err)
		}
	}

	return result, nil
}

// ensure returns the catalog object of one chart with its labels and owner reference
// up to date. present says whether the snapshot found it; an object it did not find is
// created, which is how a chart the repository has just started offering enters the
// catalog. A name the create finds taken belongs to an object the snapshot did not
// see, which is a lost race rather than a chart to publish (adopt).
//
// Metadata is written separately from the status and under the same lock, so a pass
// that loses the race repairs nothing rather than repairing it over someone else's
// object. An object that is already as it should be is not patched at all: labels and
// the owner reference change only when the repository object itself is replaced.
func (s *snapshot[C, CL]) ensure(ctx context.Context, existing C, present bool, name, chartName string) (C, error) {
	logger := log.FromContext(ctx)

	if present {
		base := existing.DeepCopyObject().(C)
		s.applyMeta(existing, chartName)

		if equality.Semantic.DeepEqual(base.GetLabels(), existing.GetLabels()) &&
			equality.Semantic.DeepEqual(base.GetOwnerReferences(), existing.GetOwnerReferences()) {
			return existing, nil
		}

		if err := s.client.Patch(ctx, existing, lockedTo(base)); err != nil {
			return existing, fmt.Errorf("updating chart %s: %w", describeKey(client.ObjectKeyFromObject(existing)), err)
		}

		logger.Info("Reconciled chart catalog object",
			"kind", s.cfg.Kind, "operation", "updated", "chartObject", client.ObjectKeyFromObject(existing))

		return existing, nil
	}

	created := s.cfg.NewObject()
	created.SetName(name)
	created.SetNamespace(s.repo.Namespace())
	s.applyMeta(created, chartName)

	err := s.client.Create(ctx, created)
	switch {
	case apierrors.IsAlreadyExists(err):
		return created, s.adopt(ctx, name, chartName)
	case err != nil:
		return created, fmt.Errorf("creating chart %s: %w", describeKey(client.ObjectKeyFromObject(created)), err)
	}

	logger.Info("Reconciled chart catalog object",
		"kind", s.cfg.Kind, "operation", "created", "chartObject", client.ObjectKeyFromObject(created))

	return created, nil
}

// adopt repairs the metadata of an object that exists on the server while this pass's
// snapshot holds none for the chart, and then reports the pass lost: the object is
// proof that the catalog moved on since the read, so nothing computed from that read
// may be written onto it.
//
// Merging into it instead would defeat the lock rather than obey it. The versions this
// pass fetched are older than whatever the writer that created this object recorded —
// it probed after our probe — and a merge patched against the object just read has the
// current resource version as its base, so the server accepts a write that puts a stale
// verdict over a fresh one. Every later pass then skips the version, because a recorded
// and unmarked verdict is exactly what a pass carries forward, and the catalog publishes
// the media type of an artifact that has already been superseded. A re-examination mark
// recorded on that object after our probe would be cleared by the same write, with the
// annotation asking for the refresh long consumed.
//
// The metadata is still repaired, under the lock like every other write of the pass: an
// object whose labels do not put it in this repository's catalog is invisible to every
// read this controller takes, so it is absent from every snapshot while occupying the
// name the chart is published under, and leaving it alone would make the collision
// permanent rather than a lost race. Labelled back in, it is part of the next snapshot
// and the retry merges into it as into any other recorded chart.
//
// The conflict is reported the way the optimistic lock's own rejection is, because it is
// the same thing: a pass computed from a read that has been overtaken, retried against
// one that has caught up (internal/reconcile/repository.Evaluate).
func (s *snapshot[C, CL]) adopt(ctx context.Context, name, chartName string) error {
	logger := log.FromContext(ctx)

	key := client.ObjectKey{Namespace: s.repo.Namespace(), Name: name}
	fetched := s.cfg.NewObject()
	if err := s.reader.Get(ctx, key, fetched); err != nil {
		return fmt.Errorf("reading chart %s: %w", describeKey(key), err)
	}

	base := fetched.DeepCopyObject().(C)
	s.applyMeta(fetched, chartName)

	if !equality.Semantic.DeepEqual(base.GetLabels(), fetched.GetLabels()) ||
		!equality.Semantic.DeepEqual(base.GetOwnerReferences(), fetched.GetOwnerReferences()) {
		if err := s.client.Patch(ctx, fetched, lockedTo(base)); err != nil {
			return fmt.Errorf("updating chart %s: %w", describeKey(key), err)
		}

		logger.Info("Reconciled chart catalog object",
			"kind", s.cfg.Kind, "operation", "adopted", "chartObject", key)
	}

	return apierrors.NewConflict(helmv1alpha1.Resource(strings.ToLower(s.cfg.Kind)), name,
		errors.New("the chart catalog object was written after this pass read the catalog"))
}

// applyMeta stamps the metadata every catalog object of a repository carries: the
// controller reference that has it garbage-collected with the repository, and the
// labels the catalog is listed and queried by.
func (s *snapshot[C, CL]) applyMeta(obj C, chartName string) {
	obj.SetOwnerReferences([]metav1.OwnerReference{
		*metav1.NewControllerRef(s.repo.Object(), s.repo.OwnerGVK()),
	})
	obj.SetLabels(map[string]string{
		helmv1alpha1.LabelDeckhouseHeritage: helmv1alpha1.LabelDeckhouseHeritageValue,
		helmv1alpha1.LabelRepositoryName:    s.repo.Name(),
		helmv1alpha1.LabelChartName:         chartName,
	})
}

// recordAbsence writes the verdict owed to a chart this pass saw the whole repository
// without listing, and which is retained only because a consumer pins a version of it.
// Nothing else reconciles such a chart: it is not among the fetched charts, so the loop
// above never merges it, and its status would otherwise keep whatever the last listing
// that did offer it left there — a media type for an artifact the repository no longer
// holds, with no unavailability reason, which is the one state that survives a chart
// being deleted and pushed again with different content behind every tag.
//
// The answer is the merge a listed chart with an empty version list would get. An
// in-use version is retained as RemovedFromRepository and keeps its media type and
// reference, which is what the pinning consumer builds its internal OCIRepository from;
// an unreferenced one is dropped, exactly as it would be for a chart still listed. The
// reason is also what makes the tag a probe candidate again the moment the chart is
// listed once more (FetchOptions.NeedsExamination), so a re-push is noticed without a
// forced refresh. And the re-examination mark — a request no pass could ever answer
// while the chart is unlisted, because probe candidates are built from the listing — is
// cleared by the same rule that clears it for a single removed version, rather than by a
// special case that clears it without answering anything.
func (s *snapshot[C, CL]) recordAbsence(ctx context.Context, chart C, inUse map[string]struct{}) error {
	base := chart.DeepCopyObject().(C)
	status := s.cfg.Status(chart)

	status.Versions = mergeChartVersions(nil, status.Versions, inUse, nil)

	if err := s.client.Status().Patch(ctx, chart, lockedTo(base)); err != nil {
		return fmt.Errorf("recording that the repository no longer offers chart %s: %w",
			describeKey(client.ObjectKeyFromObject(chart)), err)
	}

	return nil
}

// MigrateNames moves a repository's catalog objects to the names the current scheme
// derives, whatever scheme they were written under. An object is recognised by its
// chart label rather than by recomputing an older name, so this covers every scheme
// the repository has ever been reconciled with, including a chart the repository no
// longer offers and that only a consumer keeps alive: the pruning loop would keep
// such an object under its old name forever, while the consumer already resolves the
// new one.
//
// The legacy status is carried over, and the old object is deleted only once the carry
// has landed, so a failure anywhere leaves the old object in place to be migrated again
// on the next pass. The carry rests on reads that are not allowed to lag: the
// destination is read through the API server and patched under the optimistic lock,
// because what the destination already holds is a statement about the catalog rather
// than about this reader's own snapshot. A destination another pass has since written —
// marked for re-examination, say — rejects the write as a conflict, and the migration is
// then simply unfinished: the legacy object stays, and the next pass reads both objects
// again.
//
// TRANSITIONAL: remove this method, its interface entry and its call once every
// cluster has reconciled each repository at least once under the current scheme.
func (t *typed[C, CL]) MigrateNames(ctx context.Context, repo contract.Repository) error {
	logger := log.FromContext(ctx)

	existingCharts, err := t.list(ctx, repo)
	if err != nil {
		return err
	}

	for _, legacy := range existingCharts {
		chartName := legacy.GetLabels()[helmv1alpha1.LabelChartName]
		if chartName == "" {
			continue
		}

		name := t.cfg.ObjectName(repo.Name(), chartName)
		if legacy.GetName() == name {
			continue
		}

		current, err := t.destination(ctx, repo, name, legacy)
		if err != nil {
			return err
		}

		if err := t.carryOver(ctx, legacy, current); err != nil {
			return err
		}

		logger.Info("Renamed a chart catalog object",
			"kind", t.cfg.Kind, "from", client.ObjectKeyFromObject(legacy), "to", client.ObjectKeyFromObject(current), "chart", chartName)

		if err := client.IgnoreNotFound(t.client.Delete(ctx, legacy)); err != nil {
			return fmt.Errorf("deleting chart %s after renaming it: %w", describeKey(client.ObjectKeyFromObject(legacy)), err)
		}
	}

	return nil
}

// carryOver unions the legacy object into the destination: the versions the destination
// does not hold are added, because the legacy object is about to be deleted and every
// version left behind is lost; a version both hold keeps the destination's verdict, which
// is the newer one and is what this catalog publishes, but inherits an outstanding mark;
// and an icon the destination lacks is filled in.
//
// Copying the status wholesale is what the destination's own entries cannot survive, and
// a non-empty destination is not the rare case it looks like: a copy rejected by the lock
// leaves the migration unfinished, and by the retry another pass has written the
// destination, so the retry is exactly where the legacy object would be deleted with a
// version only it records. That version is not replaceable — retention exists for
// versions the repository no longer offers, and its media type is what a release pinning
// it resolves through — so it is carried over instead, next to whatever the other pass
// recorded, marks and all.
//
// A destination that already holds everything, marks included, is not patched, so the pass
// that finishes a migration nothing is left to carry writes only the deletion.
func (t *typed[C, CL]) carryOver(ctx context.Context, legacy, current C) error {
	status := t.cfg.Status(current)
	legacyStatus := t.cfg.Status(legacy)

	held := make(map[string]int, len(status.Versions))
	for i, version := range status.Versions {
		held[version.Version] = i
	}

	// The verdict the destination holds wins, but an outstanding refresh is not a verdict:
	// whichever object it was written on, nothing here can establish that it was answered,
	// and dropping it would end the refresh without doing the work it asked for. A stale
	// legacy mark therefore costs one redundant probe, which is the cheap side of the
	// trade against losing a refresh whose annotation is already consumed. The
	// indices are collected rather than applied, because the patch is computed against a
	// copy taken below and a mutation made before it would not reach the server.
	var marks []int

	missing := make([]helmv1alpha1.ChartVersion, 0, len(legacyStatus.Versions))
	for _, version := range legacyStatus.Versions {
		if i, ok := held[version.Version]; ok {
			if version.ReexaminationRequired && !status.Versions[i].ReexaminationRequired {
				marks = append(marks, i)
			}

			continue
		}

		missing = append(missing, version)
	}

	// The icon is a property of the chart rather than of one version, so it comes over
	// with the versions it describes; a destination that already has one keeps it, for
	// the same reason its versions are kept.
	icon := status.IconURL == "" && legacyStatus.IconURL != ""
	if len(missing) == 0 && !icon && len(marks) == 0 {
		return nil
	}

	base := current.DeepCopyObject().(C)

	for _, i := range marks {
		status.Versions[i].ReexaminationRequired = true
	}

	if icon {
		status.IconURL = legacyStatus.IconURL
	}

	status.Versions = append(status.Versions, missing...)
	sortChartVersions(status.Versions)

	if err := t.client.Status().Patch(ctx, current, lockedTo(base)); err != nil {
		return fmt.Errorf("carrying the status of chart %s over: %w", describeKey(client.ObjectKeyFromObject(legacy)), err)
	}

	return nil
}

// destination returns the object the migration copies into, creating it when the
// repository has none under the current name. The read is authoritative for the same
// reason the rest of the migration is: it is what the versions the destination already
// holds — the one thing that decides what is carried over at all — are judged by.
func (t *typed[C, CL]) destination(ctx context.Context, repo contract.Repository, name string, legacy C) (C, error) {
	current := t.cfg.NewObject()
	key := client.ObjectKey{Namespace: repo.Namespace(), Name: name}

	err := t.reader.Get(ctx, key, current)
	switch {
	case apierrors.IsNotFound(err):
		current = t.cfg.NewObject()
		current.SetName(name)
		current.SetNamespace(repo.Namespace())
		current.SetOwnerReferences(legacy.GetOwnerReferences())
		current.SetLabels(legacy.GetLabels())

		if err := t.client.Create(ctx, current); err != nil {
			return current, fmt.Errorf("renaming chart %s: %w", describeKey(client.ObjectKeyFromObject(legacy)), err)
		}

		return current, nil
	case err != nil:
		return current, fmt.Errorf("renaming chart %s: %w", describeKey(client.ObjectKeyFromObject(legacy)), err)
	}

	base := current.DeepCopyObject().(C)
	current.SetOwnerReferences(legacy.GetOwnerReferences())
	current.SetLabels(legacy.GetLabels())

	if equality.Semantic.DeepEqual(base.GetLabels(), current.GetLabels()) &&
		equality.Semantic.DeepEqual(base.GetOwnerReferences(), current.GetOwnerReferences()) {
		return current, nil
	}

	if err := t.client.Patch(ctx, current, lockedTo(base)); err != nil {
		return current, fmt.Errorf("renaming chart %s: %w", describeKey(client.ObjectKeyFromObject(legacy)), err)
	}

	return current, nil
}

func (t *typed[C, CL]) InUseVersions(ctx context.Context, repo contract.Repository, chartName string) (map[string]struct{}, error) {
	if chartName == "" || t.cfg.Consumers == nil {
		return nil, nil
	}

	return t.cfg.Consumers(ctx, repo, chartName)
}

func (t *typed[C, CL]) Lookup(ctx context.Context, repo contract.Repository, chartName string) (client.Object, *helmv1alpha1.ChartCatalogStatus, error) {
	obj := t.cfg.NewObject()
	key := client.ObjectKey{Namespace: repo.Namespace(), Name: t.cfg.ObjectName(repo.Name(), chartName)}

	if err := t.client.Get(ctx, key, obj); err != nil {
		return nil, nil, fmt.Errorf("getting %s %s: %w", t.cfg.Kind, key, err)
	}

	return obj, t.cfg.Status(obj), nil
}

// lockedTo builds the patch of a catalog object against the read it was computed from,
// carrying that read's resource version so the server rejects the write if the object
// moved on in between.
//
// The read every write of a pass is computed from is authoritative (Snapshot), so the
// lock is not there to catch this controller's own cache: it guards the window between
// that read and the write, which spans the whole fetch and is therefore as long as a
// pass. A version list is merged from the snapshot and a merge patch on a list ships
// the whole array, so whatever another writer put there in the meantime would be
// replaced wholesale by a state computed without it — a re-examination mark it wrote,
// for instance, with the force annotation already consumed and nothing left to ask for
// the refresh again. Reconstructing the list from a read that has been overtaken is
// wrong whatever it holds, which is why the lock sits on the write rather than on any
// one field of it.
//
// A rejected patch is a conflict, not a failure of the pass's own making: the caller
// reports it, the work queue retries, and the retry is computed from a read that has
// caught up. The catalog on the server is left exactly as the write that overtook us
// left it, which is the state the retry has to build on.
func lockedTo[C client.Object](base C) client.Patch {
	return client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})
}

// describeKey names an object in a message. A cluster-scoped object has no
// namespace, and the key's own rendering would give it a leading slash.
func describeKey(key client.ObjectKey) string {
	if key.Namespace == "" {
		return key.Name
	}

	return key.String()
}
