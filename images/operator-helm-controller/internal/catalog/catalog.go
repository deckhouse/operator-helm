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
	// Consumers reports the versions of one chart that resources of the family still
	// reference. nil means nothing in the family can reference a chart yet.
	Consumers func(ctx context.Context, repo contract.Repository, chartName string) (map[string]struct{}, error)
}

// New builds the contract.Catalog of one kind. reader reads catalog objects straight from
// the API server, where the release path (Lookup) stays on the cached client.
func New[C client.Object, CL client.ObjectList](c client.Client, reader client.Reader, cfg Config[C, CL]) contract.Catalog {
	return &typed[C, CL]{client: c, reader: reader, cfg: cfg}
}

type typed[C client.Object, CL client.ObjectList] struct {
	client client.Client
	reader client.Reader
	cfg    Config[C, CL]
}

func (t *typed[C, CL]) list(ctx context.Context, repo contract.Repository) ([]C, error) {
	// Listing by namespace keeps two same-named repositories apart; a cluster one has none.
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

// Snapshot reads every catalog object of one repository through the API server and hands
// back the state the whole pass decides from, so no two of its inputs can disagree.
func (t *typed[C, CL]) Snapshot(ctx context.Context, repo contract.Repository) (contract.CatalogSnapshot, error) {
	charts, err := t.list(ctx, repo)
	if err != nil {
		return nil, err
	}

	logger := log.FromContext(ctx)
	for _, chart := range charts {
		if chart.GetLabels()[helmv1alpha1.LabelChartName] == "" {
			// The chart label is the only way back from the object name to the chart name.
			logger.Info("Chart object has no chart label, dropping its recorded verdicts",
				"kind", t.cfg.Kind, "chartObject", client.ObjectKeyFromObject(chart))
		}
	}

	return &snapshot[C, CL]{typed: t, repo: repo, charts: charts}, nil
}

type snapshot[C client.Object, CL client.ObjectList] struct {
	*typed[C, CL]

	repo   contract.Repository
	charts []C
}

// Known collects the verdicts recorded by previous passes, so the client can skip the tags
// it has already examined. It is computed from the snapshot each time it is asked for.
func (s *snapshot[C, CL]) Known() repoclient.KnownCharts {
	known := make(repoclient.KnownCharts, len(s.charts))

	for _, chart := range s.charts {
		chartName := chart.GetLabels()[helmv1alpha1.LabelChartName]
		if chartName == "" {
			// Reported by the read that built this snapshot; the verdicts have no key to sit under.
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

// MarkForReexamination marks every verdict the catalog holds for the repository as owing a
// fresh one. The mark is not a verdict: an entry keeps its media type and stays deployable.
func (s *snapshot[C, CL]) MarkForReexamination(ctx context.Context) error {
	for _, chart := range s.charts {
		base := chart.DeepCopyObject().(C)
		status := s.cfg.Status(chart)

		patch := false

		for i := range status.Versions {
			// A version already awaiting a verdict is re-examined by every pass anyway.
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
	// A fetch that ran out of its budget saw only part of the repository, so it may prune nothing.
	incomplete := false

	for _, chart := range charts {
		if len(chart.UnexaminedVersions) > 0 {
			incomplete = true
		}

		if errs := validation.IsValidLabelValue(chart.Name); len(errs) > 0 {
			// The chart label carries the name verbatim, so a name the API cannot hold gets no object.
			logger.V(1).Info("Skipping a chart whose name cannot be recorded on a catalog object",
				"kind", s.cfg.Kind, "chart", chart.Name, "reason", strings.Join(errs, "; "))

			result.SkippedCharts = append(result.SkippedCharts, chart.Name)

			continue
		}

		name := s.cfg.ObjectName(s.repo.Name(), chart.Name)
		desired[name] = struct{}{}

		// A chart with no usable version is still published, or the pruning below would delete it.
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

	// Pruned against the same read the rest of the pass used; an object created after it is left alone.
	for _, chart := range s.charts {
		if _, wanted := desired[chart.GetName()]; wanted {
			continue
		}

		chartName := chart.GetLabels()[helmv1alpha1.LabelChartName]
		if chartName == "" {
			// Without the chart label the in-use protection cannot be checked, so this fails open.
			logger.Info("Pruning a chart with no chart label; in-use protection could not be checked",
				"kind", s.cfg.Kind, "chartObject", client.ObjectKeyFromObject(chart))
		}

		inUse, err := s.InUseVersions(ctx, s.repo, chartName)
		if err != nil {
			return result, err
		}
		if len(inUse) > 0 {
			// A consumer still references this chart: deleting it would block every change to the consumer.
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

func (s *snapshot[C, CL]) ensure(ctx context.Context, existing C, present bool, name, chartName string) (C, error) {
	logger := log.FromContext(ctx)

	if present {
		base := existing.DeepCopyObject().(C)
		applyMeta(existing, s.repo, chartName)

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
	applyMeta(created, s.repo, chartName)

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

func (s *snapshot[C, CL]) adopt(ctx context.Context, name, chartName string) error {
	// The metadata is repaired, but the pass is reported lost: the catalog moved on since its read.
	logger := log.FromContext(ctx)

	key := client.ObjectKey{Namespace: s.repo.Namespace(), Name: name}
	fetched := s.cfg.NewObject()
	if err := s.reader.Get(ctx, key, fetched); err != nil {
		return fmt.Errorf("reading chart %s: %w", describeKey(key), err)
	}

	base := fetched.DeepCopyObject().(C)
	applyMeta(fetched, s.repo, chartName)

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

func applyMeta(obj client.Object, repo contract.Repository, chartName string) {
	obj.SetOwnerReferences([]metav1.OwnerReference{
		*metav1.NewControllerRef(repo.Object(), repo.OwnerGVK()),
	})
	obj.SetLabels(map[string]string{
		helmv1alpha1.LabelDeckhouseHeritage: helmv1alpha1.LabelDeckhouseHeritageValue,
		helmv1alpha1.LabelRepositoryName:    repo.Name(),
		helmv1alpha1.LabelChartName:         chartName,
	})
}

func (s *snapshot[C, CL]) recordAbsence(ctx context.Context, chart C, inUse map[string]struct{}) error {
	base := chart.DeepCopyObject().(C)
	status := s.cfg.Status(chart)

	// The merge a listed chart with an empty version list would get.
	status.Versions = mergeChartVersions(nil, status.Versions, inUse, nil)

	if err := s.client.Status().Patch(ctx, chart, lockedTo(base)); err != nil {
		return fmt.Errorf("recording that the repository no longer offers chart %s: %w",
			describeKey(client.ObjectKeyFromObject(chart)), err)
	}

	return nil
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
		return nil, nil, fmt.Errorf("getting %s %s: %w", t.cfg.Kind, describeKey(key), err)
	}

	return obj, t.cfg.Status(obj), nil
}

func lockedTo[C client.Object](base C) client.Patch {
	return client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})
}

func describeKey(key client.ObjectKey) string {
	if key.Namespace == "" {
		return key.Name
	}

	return key.String()
}
