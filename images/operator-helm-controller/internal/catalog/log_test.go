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

package catalog_test

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"github.com/deckhouse/operator-helm/internal/adapter"
	repoclient "github.com/deckhouse/operator-helm/internal/client/repository"
)

func TestCatalogLogsDoNotShadowTheReconciledObject(t *testing.T) {
	var out bytes.Buffer

	logger := zap.New(zap.UseDevMode(false), zap.WriteTo(&out)).WithValues(
		"controller", "helmapplicationrepository-controller",
		"controllerGroup", "helm.deckhouse.io",
		"controllerKind", "HelmApplicationRepository",
		"HelmApplicationRepository", klog.KRef("team-a", "stable"),
		"namespace", "team-a", "name", "stable", "reconcileID", "r-1",
	)
	ctx := log.IntoContext(context.Background(), logger)

	c := newClient(t)
	repo := applicationRepo("team-a", "stable")

	snapshot, err := adapter.NewApplicationCatalog(c, c).Snapshot(ctx, repo)
	if err != nil {
		t.Fatalf("reading the catalog returned %v", err)
	}
	if _, err := snapshot.Reconcile(ctx, []repoclient.Chart{chart("podinfo", "6.7.1")}); err != nil {
		t.Fatalf("Reconcile returned %v", err)
	}

	if out.Len() == 0 {
		t.Fatal("the reconcile logged nothing, so the assertion below would prove nothing")
	}

	for _, line := range bytes.Split(bytes.TrimSpace(out.Bytes()), []byte("\n")) {
		var fields map[string]any
		if err := json.Unmarshal(line, &fields); err != nil {
			t.Fatalf("log line is not JSON: %v", err)
		}

		if got := fields["name"]; got != "stable" {
			t.Errorf("log line %q reports name %q, want the reconciled repository %q",
				fields["msg"], got, "stable")
		}
		if got := fields["namespace"]; got != "team-a" {
			t.Errorf("log line %q reports namespace %q, want %q", fields["msg"], got, "team-a")
		}
	}
}
