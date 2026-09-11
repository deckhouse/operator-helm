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

	if err := t.migrateNames(ctx, repo); err != nil {
		return err
	}

	desired := make(map[string]struct{}, len(charts))

	for _, chart := range charts {
		name := t.cfg.ObjectName(repo.Name(), chart.Name)
		// A chart with no usable version is still created: it carries the reason each of
		// its versions is unusable, and skipping it here would let the pruning loop below
		// delete a chart whose tags merely failed to resolve.
		existing := t.cfg.NewObject()
		existing.SetName(name)
		existing.SetNamespace(repo.Namespace())

		desired[name] = struct{}{}

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
		if len(chart.Versions) > 0 {
			status.IconURL = chart.Versions[0].IconURL
		}
		status.Versions = mergeChartVersions(chart.Versions, status.Versions, inUse)

		if err := t.client.Status().Patch(ctx, existing, client.MergeFrom(base)); err != nil {
			return fmt.Errorf("updating versions of chart %s: %w", describeKey(client.ObjectKeyFromObject(existing)), err)
		}
	}

	existingCharts, err := t.list(ctx, repo)
	if err != nil {
		return fmt.Errorf("listing charts for pruning: %w", err)
	}

	for _, chart := range existingCharts {
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

// migrateNames moves a repository's catalog objects to the names the current scheme
// derives, whatever scheme they were written under. An object is recognised by its
// chart label rather than by recomputing an older name, so this covers every scheme
// the repository has ever been reconciled with, including a chart the repository no
// longer offers and that only a consumer keeps alive: the pruning loop would keep
// such an object under its old name forever, while the consumer already resolves the
// new one.
//
// The status is copied only into an object that has none, and the old object is
// deleted only once the copy has landed, so a failure anywhere leaves the old object
// in place to be migrated again on the next pass.
//
// TRANSITIONAL: remove this method and its call once every cluster has reconciled
// each repository at least once under the current scheme.
func (t *typed[C, CL]) migrateNames(ctx context.Context, repo source.Repository) error {
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

		current := t.cfg.NewObject()
		current.SetName(name)
		current.SetNamespace(repo.Namespace())

		if _, err := controllerutil.CreateOrPatch(ctx, t.client, current, func() error {
			current.SetOwnerReferences(legacy.GetOwnerReferences())
			current.SetLabels(legacy.GetLabels())

			return nil
		}); err != nil {
			return fmt.Errorf("renaming chart %s: %w", describeKey(client.ObjectKeyFromObject(legacy)), err)
		}

		if status := t.cfg.Status(current); len(status.Versions) == 0 {
			base := current.DeepCopyObject().(C)
			*status = *t.cfg.Status(legacy)

			if err := t.client.Status().Patch(ctx, current, client.MergeFrom(base)); err != nil {
				return fmt.Errorf("carrying the status of chart %s over: %w", describeKey(client.ObjectKeyFromObject(legacy)), err)
			}
		}

		logger.Info("Renamed a chart catalog object", "kind", t.cfg.Kind, "from", legacy.GetName(), "to", name, "chart", chartName)

		if err := client.IgnoreNotFound(t.client.Delete(ctx, legacy)); err != nil {
			return fmt.Errorf("deleting chart %s after renaming it: %w", describeKey(client.ObjectKeyFromObject(legacy)), err)
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
