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

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/deckhouse/operator-helm/internal/source"
)

var _ source.TargetNamespaceEnsurer = (*NamespaceService)(nil)

// NamespaceService creates the namespace an addon deploys into when it does not
// exist yet. It never modifies an existing namespace: the namespace belongs to
// whoever created it.
type NamespaceService struct {
	client client.Client
}

func NewNamespaceService(c client.Client) *NamespaceService {
	return &NamespaceService{client: c}
}

func (s *NamespaceService) EnsureTargetNamespace(ctx context.Context, rel source.Release) error {
	ns := &corev1.Namespace{}

	err := s.client.Get(ctx, client.ObjectKey{Name: rel.TargetNamespace()}, ns)
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("getting namespace: %w", err)
	}

	ns = &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: rel.TargetNamespace(),
		},
	}

	if err := s.client.Create(ctx, ns); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("creating namespace: %w", err)
	}

	return nil
}
