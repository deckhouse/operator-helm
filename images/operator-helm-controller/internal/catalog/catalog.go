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
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
	repoclient "github.com/deckhouse/operator-helm/internal/client/repository"
	"github.com/deckhouse/operator-helm/internal/source"
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
	Consumers func(ctx context.Context, repo source.Repository, chartName string) (map[string]struct{}, error)
}

// New builds the source.Catalog of one kind.
func New[C client.Object, CL client.ObjectList](c client.Client, cfg Config[C, CL]) source.Catalog {
	return &typed[C, CL]{client: c, cfg: cfg}
}

type typed[C client.Object, CL client.ObjectList] struct {
	client client.Client
	cfg    Config[C, CL]
}

// list returns the catalog objects of one repository. A namespaced repository's
// objects live in its namespace; for a cluster repository the namespace is empty and
// InNamespace is a no-op. Listing by namespace is what keeps two same-named
// repositories in different namespaces apart.
func (t *typed[C, CL]) list(ctx context.Context, repo source.Repository) ([]C, error) {
	list := t.cfg.NewList()
	if err := t.client.List(ctx, list,
		client.InNamespace(repo.Namespace()),
		client.MatchingLabels{helmv1alpha1.LabelRepositoryName: repo.Name()},
	); err != nil {
		return nil, fmt.Errorf("listing %s objects of repository %s: %w",
			t.cfg.Kind, describeKey(client.ObjectKey{Namespace: repo.Namespace(), Name: repo.Name()}), err)
	}

	return t.cfg.Items(list), nil
}

// Known collects the verdicts recorded by previous passes, so the client can skip
// the tags it has already examined. The chart objects are the only store of that
// state: keeping a separate fingerprint would be one more thing to drift.
func (t *typed[C, CL]) Known(ctx context.Context, repo source.Repository) (repoclient.KnownCharts, error) {
	charts, err := t.list(ctx, repo)
	if err != nil {
		return nil, err
	}

	logger := log.FromContext(ctx)
	known := make(repoclient.KnownCharts, len(charts))

	for _, chart := range charts {
		chartName := chart.GetLabels()[helmv1alpha1.LabelChartName]
		if chartName == "" {
			// The chart label is the only way back from the object name (a
			// truncated hash) to the chart name it belongs to. Without it the
			// recorded verdicts for this chart cannot be looked up here, so every
			// tag is re-examined on the next fetch; that is safe but not free, so
			// it is worth surfacing.
			logger.Info("Chart object has no chart label, dropping its recorded verdicts", "kind", t.cfg.Kind, "name", chart.GetName())

			continue
		}

		recorded := t.cfg.Status(chart).Versions
		versions := make(repoclient.KnownVersions, len(recorded))
		for _, version := range recorded {
			versions[version.Version] = repoclient.KnownVersion{
				MediaType:          version.MediaType,
				UnavailableReason:  version.UnavailableReason,
				UnavailableMessage: version.UnavailableMessage,
			}
		}

		known[chartName] = versions
	}

	return known, nil
}

func (t *typed[C, CL]) Reconcile(ctx context.Context, repo source.Repository, charts []repoclient.Chart) error {
	logger := log.FromContext(ctx)

	desired := make(map[string]struct{}, len(charts))
	desiredNameByChart := make(map[string]string, len(charts))

	for _, chart := range charts {
		name := t.cfg.ObjectName(repo.Name(), chart.Name)
		desired[name] = struct{}{}
		desiredNameByChart[chart.Name] = name
	}

	// The pruning loop below needs this same listing, so it is fetched once, before
	// any object is created or patched: that is also what lets the loop find a chart
	// object still sitting under an earlier naming scheme, for the migration below.
	existingCharts, err := t.list(ctx, repo)
	if err != nil {
		return fmt.Errorf("listing charts for pruning: %w", err)
	}

	// TRANSITIONAL: chartObjectName became injective for every catalog kind, moving
	// every object to a new name. legacyByChart maps a chart being reconciled to one
	// object still sitting under its old name, found by the chart label rather than
	// by recomputing the old scheme, so this also covers any earlier scheme. Remove
	// this map and its two uses below, and the "an earlier naming scheme" branch in
	// the pruning loop, once every cluster has synchronized under the new scheme at
	// least once.
	legacyByChart := make(map[string]C, len(existingCharts))
	for _, existing := range existingCharts {
		chartName := existing.GetLabels()[helmv1alpha1.LabelChartName]
		if wantName, reconciling := desiredNameByChart[chartName]; reconciling && existing.GetName() != wantName {
			legacyByChart[chartName] = existing
		}
	}

	for _, chart := range charts {
		name := desiredNameByChart[chart.Name]
		// A chart with no usable version is still created: it carries the reason each of
		// its versions is unusable, and skipping it here would let the pruning loop below
		// delete a chart whose tags merely failed to resolve.
		existing := t.cfg.NewObject()
		existing.SetName(name)
		existing.SetNamespace(repo.Namespace())

		op, err := controllerutil.CreateOrPatch(ctx, t.client, existing, func() error {
			existing.SetOwnerReferences([]metav1.OwnerReference{
				*metav1.NewControllerRef(repo.Object(), repo.OwnerGVK()),
			})
			existing.SetLabels(map[string]string{
				helmv1alpha1.LabelDeckhouseHeritage: helmv1alpha1.LabelDeckhouseHeritageValue,
				helmv1alpha1.LabelRepositoryName:    repo.Name(),
				helmv1alpha1.LabelChartName:         chart.Name,
			})

			return nil
		})
		if err != nil {
			return fmt.Errorf("creating or updating chart %s: %w", describeKey(client.ObjectKeyFromObject(existing)), err)
		}

		if op != controllerutil.OperationResultNone {
			logger.Info("Reconciled chart catalog object", "kind", t.cfg.Kind, "operation", op, "name", name)
		}

		inUse, err := t.InUseVersions(ctx, repo, chart.Name)
		if err != nil {
			return err
		}

		base := existing.DeepCopyObject().(C)
		status := t.cfg.Status(existing)

		// TRANSITIONAL: a freshly created object starts with an empty status, which
		// would otherwise make mergeChartVersions forget a version a consumer still
		// references. Seeding only the "current" input mergeChartVersions reads
		// (base above must still mirror the object's real, empty server state, or
		// the status patch below would not carry the seeded fields at all) replays
		// that protection once, from whatever the object carried under its old name.
		currentVersions := status.Versions
		if op == controllerutil.OperationResultCreated {
			if legacy, ok := legacyByChart[chart.Name]; ok {
				currentVersions = t.cfg.Status(legacy).Versions
			}
		}

		if len(chart.Versions) > 0 {
			status.IconURL = chart.Versions[0].IconURL
		}
		status.Versions = mergeChartVersions(chart.Versions, currentVersions, inUse)

		if err := t.client.Status().Patch(ctx, existing, client.MergeFrom(base)); err != nil {
			return fmt.Errorf("updating versions of chart %s: %w", describeKey(client.ObjectKeyFromObject(existing)), err)
		}
	}

	for _, chart := range existingCharts {
		if _, wanted := desired[chart.GetName()]; wanted {
			continue
		}

		chartName := chart.GetLabels()[helmv1alpha1.LabelChartName]

		if _, reconciling := desiredNameByChart[chartName]; reconciling {
			// TRANSITIONAL: this object's chart now lives under the name reconciled
			// above, so it is a leftover of an earlier naming scheme rather than a
			// chart the repository stopped offering. The in-use check below exists to
			// protect a chart a consumer still needs, but the consumer now resolves
			// to the new name, so this leftover is deleted unconditionally.
			logger.Info("Deleting a chart object left over from an earlier naming scheme", "kind", t.cfg.Kind, "name", chart.GetName(), "chart", chartName)

			if err := client.IgnoreNotFound(t.client.Delete(ctx, chart)); err != nil {
				return fmt.Errorf("deleting a chart object from an earlier naming scheme: %w", err)
			}

			continue
		}

		if chartName == "" {
			// The chart label is the only way back from the object name (a
			// truncated hash) to the chart name a consumer references, so
			// InUseVersions cannot find anything to protect and this chart is
			// pruned even if a consumer still uses it. That fail-open is unavoidable
			// as written, so at least make it diagnosable.
			logger.Info("Pruning a chart with no chart label; in-use protection could not be checked", "kind", t.cfg.Kind, "name", chart.GetName())
		}

		inUse, err := t.InUseVersions(ctx, repo, chartName)
		if err != nil {
			return err
		}
		if len(inUse) > 0 {
			// A consumer still references this chart: deleting the object would make
			// its own reconciliation fail on a missing chart and block every change
			// to it, including its removal.
			logger.Info("Keeping a chart referenced by a consumer", "kind", t.cfg.Kind, "name", chart.GetName())

			continue
		}

		if err := client.IgnoreNotFound(t.client.Delete(ctx, chart)); err != nil {
			return fmt.Errorf("deleting stale charts: %w", err)
		}
	}

	return nil
}

func (t *typed[C, CL]) InUseVersions(ctx context.Context, repo source.Repository, chartName string) (map[string]struct{}, error) {
	if chartName == "" || t.cfg.Consumers == nil {
		return nil, nil
	}

	return t.cfg.Consumers(ctx, repo, chartName)
}

func (t *typed[C, CL]) Lookup(ctx context.Context, repo source.Repository, chartName string) (client.Object, *helmv1alpha1.ChartCatalogStatus, error) {
	obj := t.cfg.NewObject()
	key := client.ObjectKey{Namespace: repo.Namespace(), Name: t.cfg.ObjectName(repo.Name(), chartName)}

	if err := t.client.Get(ctx, key, obj); err != nil {
		return nil, nil, fmt.Errorf("getting %s %s: %w", t.cfg.Kind, key, err)
	}

	return obj, t.cfg.Status(obj), nil
}

// describeKey names an object in a message. A cluster-scoped object has no
// namespace, and the key's own rendering would give it a leading slash.
func describeKey(key client.ObjectKey) string {
	if key.Namespace == "" {
		return key.Name
	}

	return key.String()
}
