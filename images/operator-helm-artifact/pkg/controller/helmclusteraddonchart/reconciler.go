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

package helmclusteraddonchart

import (
	"context"
	"fmt"

	sourcev1 "github.com/werf/nelm-source-controller/api/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
)

type reconciler struct {
	client client.Client
}

func (r *reconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	logger := log.FromContext(ctx).WithValues("helmclusteraddonchart", req.Name)

	chart := &helmv1alpha1.HelmClusterAddonChart{}
	if err := r.client.Get(ctx, req.NamespacedName, chart); client.IgnoreNotFound(err) != nil {
		return ctrl.Result{}, fmt.Errorf("failed to get HelmClusterAddonChart: %w", err)
	}

	if !chart.DeletionTimestamp.IsZero() {
		return reconcile.Result{}, nil
	}

	base := chart.DeepCopy()

	internalCharts := &sourcev1.HelmChartList{}
	if err := r.client.List(ctx, internalCharts, client.InNamespace(TargetNamespace)); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to list internal helm chart list: %w", err)
	}

	updateRequired := false
	for i, v := range chart.Status.Versions {
		found := false
		for _, child := range internalCharts.Items {
			if child.Spec.Version == v.Version && child.Status.Artifact != nil {
				found = true
				break
			}
		}

		if chart.Status.Versions[i].Pulled != found {
			chart.Status.Versions[i].Pulled = found
			updateRequired = true
		}
	}

	if updateRequired {
		if err := r.client.Status().Patch(ctx, chart, client.MergeFrom(base)); err != nil {
			return reconcile.Result{}, fmt.Errorf("failed to update HelmClusterAddonChart status: %w", err)
		}

		logger.Info("HelmClusterAddonChart successfully reconciled")
	}

	return reconcile.Result{}, nil
}
