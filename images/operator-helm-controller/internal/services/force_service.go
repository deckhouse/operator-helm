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

package services

import (
	"context"
	"fmt"

	sourcev1 "github.com/werf/nelm-source-controller/api/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/deckhouse/operator-helm/internal/source"
)

var _ source.ConsumerForcer = (*ForceService)(nil)

// ForceService pushes a repository's force reconcile request onto the internal
// OCIRepository of every release consuming it. Which releases those are is the
// family's business: the lister is injected.
type ForceService struct {
	client          client.Client
	targetNamespace string
	list            source.ReleaseLister
}

func NewForceService(c client.Client, targetNamespace string, list source.ReleaseLister) *ForceService {
	return &ForceService{client: c, targetNamespace: targetNamespace, list: list}
}

// ForceReconcileConsumers stamps the reconcile request annotations on the internal
// OCIRepository of every release that references repo.
//
// An artifact pulled per release has no internal source object shared by the
// repository, so a force request reaches it only through the releases' own
// OCIRepositories. That is every release of an oci:// repository, and every release
// of a helm repository whose version the index publishes in a registry.
//
// A release whose internal OCIRepository does not exist yet is skipped: the force
// request must not be blocked by a release that has not reached the point of
// building one.
func (s *ForceService) ForceReconcileConsumers(ctx context.Context, repo source.Repository) error {
	releases, err := s.list(ctx, repo, "")
	if err != nil {
		return err
	}

	for _, rel := range releases {
		name := rel.InternalNames().OCIRepository
		nn := types.NamespacedName{Name: name, Namespace: s.targetNamespace}

		ociRepo := &sourcev1.OCIRepository{}
		if err := s.client.Get(ctx, nn, ociRepo); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}

			return fmt.Errorf("getting internal oci repository %s: %w", name, err)
		}

		base := ociRepo.DeepCopy()
		setReconcileRequestAnnotations(ociRepo)

		// The internal repository may be removed between the get and the patch,
		// which is the same case as the one skipped above.
		if err := s.client.Patch(ctx, ociRepo, client.MergeFrom(base)); client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("requesting reconciliation of internal oci repository %s: %w", name, err)
		}
	}

	return nil
}
