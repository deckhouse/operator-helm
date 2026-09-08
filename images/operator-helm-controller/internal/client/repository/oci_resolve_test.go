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

package repository

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
)

func TestResolveChartArtifactSupportedLayers(t *testing.T) {
	_, host := newFakeRegistry(t)
	pushChart(t, host, "1.0.0", helmConfigMediaType, currentChartMediaType)
	pushChart(t, host, "1.0.1", helmConfigMediaType, legacyChartMediaType)

	for tag, want := range map[string]string{
		"1.0.0": string(currentChartMediaType),
		"1.0.1": string(legacyChartMediaType),
	} {
		got, err := OCIChartResolverDefault.ResolveChartArtifact(
			context.Background(), "oci://"+host+"/podinfo:"+tag, nil,
		)
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", tag, err)
		}
		if got != want {
			t.Fatalf("%s: media type = %q, want %q", tag, got, want)
		}
	}
}

func TestResolveChartArtifactRejectsNonChart(t *testing.T) {
	_, host := newFakeRegistry(t)
	pushChart(t, host, "1.0.0", "application/vnd.unknown.config.v1+json", legacyChartMediaType)

	_, err := OCIChartResolverDefault.ResolveChartArtifact(
		context.Background(), "oci://"+host+"/podinfo:1.0.0", nil,
	)
	if err == nil {
		t.Fatal("an artifact that is not a chart must be rejected")
	}

	terminal, ok := AsTerminal(err)
	if !ok {
		t.Fatalf("error %v must be terminal: the artifact will not become a chart by retrying", err)
	}
	if terminal.Reason != helmv1alpha1.ReasonUnsupportedChartArtifact {
		t.Fatalf("reason = %q, want %q", terminal.Reason, helmv1alpha1.ReasonUnsupportedChartArtifact)
	}
}

func TestResolveChartArtifactRejectsIndex(t *testing.T) {
	_, host := newFakeRegistry(t)

	index, err := random.Index(1, 1, 1)
	if err != nil {
		t.Fatalf("building an index: %v", err)
	}
	ref, err := name.NewTag(host + "/podinfo:1.0.0")
	if err != nil {
		t.Fatalf("parsing tag: %v", err)
	}
	if err := remote.WriteIndex(ref, index); err != nil {
		t.Fatalf("pushing index: %v", err)
	}

	_, err = OCIChartResolverDefault.ResolveChartArtifact(
		context.Background(), "oci://"+host+"/podinfo:1.0.0", nil,
	)
	if _, ok := AsTerminal(err); !ok {
		t.Fatalf("error %v must be terminal: an index is never a chart", err)
	}
}

func TestResolveChartArtifactMissingTagIsTerminal(t *testing.T) {
	_, host := newFakeRegistry(t)

	_, err := OCIChartResolverDefault.ResolveChartArtifact(
		context.Background(), "oci://"+host+"/podinfo:1.0.0", nil,
	)

	terminal, ok := AsTerminal(err)
	if !ok {
		t.Fatalf("error %v must be terminal: the index promises a tag the registry does not have", err)
	}
	if terminal.Reason != helmv1alpha1.ReasonSourceNotFound {
		t.Fatalf("reason = %q, want %q", terminal.Reason, helmv1alpha1.ReasonSourceNotFound)
	}
}

// TestResolveChartArtifactRejectionIsRetriable is the deliberate difference from
// FetchCharts: there a 401/403 is escalated to a terminal failure of the whole pass,
// because credentials rejected for one tag are rejected for all of them. Here the
// pass is a single tag on a registry the repository merely names, and a rejection is
// as likely to be a rate limit as a permission problem.
func TestResolveChartArtifactRejectionIsRetriable(t *testing.T) {
	reg, host := newFakeRegistry(t)
	pushChart(t, host, "1.0.0", helmConfigMediaType, currentChartMediaType)
	reg.force("1.0.0", http.StatusUnauthorized)

	_, err := OCIChartResolverDefault.ResolveChartArtifact(
		context.Background(), "oci://"+host+"/podinfo:1.0.0", nil,
	)
	if err == nil {
		t.Fatal("a rejected request must be reported as an error")
	}
	if _, ok := AsTerminal(err); ok {
		t.Fatalf("error %v must stay retriable", err)
	}
}

func TestResolveChartArtifactUnparsableReferenceIsTerminal(t *testing.T) {
	_, err := OCIChartResolverDefault.ResolveChartArtifact(context.Background(), "oci://BAD_HOST//:::", nil)
	if _, ok := AsTerminal(err); !ok {
		t.Fatalf("error %v must be terminal: the reference will not become valid by retrying", err)
	}
}
