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

package internalobj

import (
	"context"
	"fmt"

	sourcev1 "github.com/fluxcd/source-controller/api/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
	"github.com/deckhouse/operator-helm/internal/contract"
	"github.com/deckhouse/operator-helm/internal/utils"
)

// internalChartInterval is how often source-controller re-checks the chart against
// the HelmRepository it is built from.
const internalChartInterval = InternalRepositoryInterval // Unset is sent as "0s", which controller-runtime reads as "do not requeue".

var helmChartErrorRules = []ErrorConditionRule{
	{Type: "FetchFailed", TriggerStatus: metav1.ConditionTrue, Reason: helmv1alpha1.ReasonChartFetchFailed},
	{Type: "StorageOperationFailed", TriggerStatus: metav1.ConditionTrue, Reason: helmv1alpha1.ReasonChartStorageFailed},
}

type Charts struct {
	client          client.Client
	targetNamespace string
}

func NewCharts(client client.Client, targetNamespace string) *Charts {
	return &Charts{client: client, targetNamespace: targetNamespace}
}

func (s *Charts) Ensure(ctx context.Context, rel contract.Release, repo contract.Repository) ChartOutcome {
	logger := log.FromContext(ctx)

	existing := &sourcev1.HelmChart{
		ObjectMeta: metav1.ObjectMeta{
			Name:      rel.InternalNames().HelmChart,
			Namespace: s.targetNamespace,
		},
	}

	op, err := controllerutil.CreateOrPatch(ctx, s.client, existing, func() error {
		applyHelmChartSpec(rel, repo, existing)

		return nil
	})
	if err != nil {
		return ChartOutcome{Err: fmt.Errorf("creating or updating helm chart: %w", err)}
	}

	if op != controllerutil.OperationResultNone {
		logger.Info("Reconciled helm chart", "operation", op,
			"internalObject", client.ObjectKeyFromObject(existing))
	}

	internal := reduceInternalConditions(existing.GetConditions(), existing.Generation, helmChartErrorRules)

	if internal.Ready() {
		logger.Info("Successfully reconciled helm chart", "operation", op, "chart", rel.ChartRef().Chart,
			"internalObject", client.ObjectKeyFromObject(existing))
	}

	return ChartOutcome{
		Artifact: existing.Status.Artifact,
		Internal: internal,
	}
}

// Cleanup issues a delete for the internal HelmChart and returns it while it is still
// present, so the caller can wait for source-controller to finish removing it.
func (s *Charts) Cleanup(ctx context.Context, names contract.ReleaseNames) (*sourcev1.HelmChart, error) {
	nn := types.NamespacedName{Name: names.HelmChart, Namespace: s.targetNamespace}
	chart := &sourcev1.HelmChart{}
	exists, err := utils.DeleteIfPresent(ctx, s.client, nn, chart)
	if err != nil {
		return nil, fmt.Errorf("failed to delete helm chart: %w", err)
	}
	if !exists {
		return nil, nil
	}

	return chart, nil
}

func applyHelmChartSpec(rel contract.Release, repo contract.Repository, existing *sourcev1.HelmChart) {
	if rel.ForceReconcileRequired() {
		setReconcileRequestAnnotations(existing)
	}

	applyOwnedLabels(existing, rel.HelmChartLabels())

	ref := rel.ChartRef()
	existing.Spec.Chart = ref.Chart
	existing.Spec.Version = ref.Version
	existing.Spec.Interval = metav1.Duration{Duration: internalChartInterval}

	existing.Spec.SourceRef = sourcev1.LocalHelmChartSourceReference{
		Kind: sourcev1.HelmRepositoryKind,
		Name: repo.InternalNames().HelmRepository,
	}
}
