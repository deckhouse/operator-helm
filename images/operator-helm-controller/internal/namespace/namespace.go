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

package namespace

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/deckhouse/operator-helm/internal/contract"
)

// Namespaces is the release reconciler's Namespaces: it creates a
// release's target namespace when it does not exist yet, and never modifies an
// existing one — the namespace belongs to whoever created it. Today only the addon
// controller wires it in; other families deploy into a namespace that must already
// exist (their own), which ExistingTarget below serves.
type Namespaces struct {
	// reader reads from the API server directly (mgr.GetAPIReader()), bypassing the
	// controller cache: nothing watches Namespaces, so a cached typed Get would start
	// an informer the ClusterRole has no watch permission for.
	reader client.Reader
	client client.Client
}

func NewNamespaces(c client.Client, reader client.Reader) *Namespaces {
	return &Namespaces{client: c, reader: reader}
}

func (s *Namespaces) EnsureTarget(ctx context.Context, rel contract.Release) error {
	ns := &corev1.Namespace{}

	err := s.reader.Get(ctx, client.ObjectKey{Name: rel.TargetNamespace()}, ns)
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

func (s *Namespaces) Terminating(ctx context.Context, rel contract.Release) (bool, error) {
	return terminating(ctx, s.reader, rel.TargetNamespace())
}

// ExistingTarget is the Namespaces of a family whose target namespace is the
// release's own: it exists by definition, or the release could not, and this
// collaborator never creates one. It still has to answer whether that namespace is
// on its way out, which is what makes an application's teardown different from an
// ordinary deletion.
type ExistingTarget struct {
	// reader reads from the API server directly, for the same reason Namespaces does:
	// nothing watches Namespaces, so a cached typed Get would start an informer the
	// ClusterRole has no watch permission for — and whether a namespace is terminating
	// must not be answered from a cache that may predate the deletion.
	reader client.Reader
}

func NewExistingTarget(reader client.Reader) *ExistingTarget {
	return &ExistingTarget{reader: reader}
}

func (*ExistingTarget) EnsureTarget(context.Context, contract.Release) error { return nil }

func (s *ExistingTarget) Terminating(ctx context.Context, rel contract.Release) (bool, error) {
	return terminating(ctx, s.reader, rel.TargetNamespace())
}

// terminating reports whether the namespace is being deleted or is already gone.
// A namespace that is not there at all counts as terminating: everything it held
// is gone with it, which is the very state the callers of this act on. A read that
// fails for any other reason is handed back — the answer decides whether a release
// tears itself down differently, and guessing it from a passing API error would
// make that decision at random.
func terminating(ctx context.Context, reader client.Reader, name string) (bool, error) {
	ns := &corev1.Namespace{}
	if err := reader.Get(ctx, client.ObjectKey{Name: name}, ns); err != nil {
		if apierrors.IsNotFound(err) {
			return true, nil
		}

		return false, fmt.Errorf("getting namespace: %w", err)
	}

	return !ns.DeletionTimestamp.IsZero(), nil
}
