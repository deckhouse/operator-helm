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
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"

	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
)

var OCIChartResolverDefault ChartResolverInterface = &ociChartResolver{}

type ociChartResolver struct{}

// Bounds the whole probe: both attempts and the pause between them. A variable so tests need not wait.
var chartArtifactProbeBudget = 30 * time.Second

// ResolveChartArtifact reads one manifest and applies the same "is this a chart" rule the
// catalog client applies. config carries transport settings only: its credentials are ignored.
func (r *ociChartResolver) ResolveChartArtifact(ctx context.Context, ref string, config *RepoConfig) (string, error) {
	tag, err := name.NewTag(trimSchemaPrefixes(ref))
	if err != nil {
		return "", &TerminalError{
			Reason:  helmv1alpha1.ReasonInvalidRepositoryURL,
			Message: fmt.Sprintf("chart reference %q cannot be parsed", ref),
			Err:     err,
		}
	}

	ctx, cancel := context.WithTimeout(ctx, chartArtifactProbeBudget)
	defer cancel()

	options := []remote.Option{
		remote.WithContext(ctx),
		remote.WithUserAgent("operator-helm-controller"),
		// Two attempts: a longer backoff here would hold the addon's reconciliation open.
		remote.WithRetryBackoff(remote.Backoff{
			Duration: 1 * time.Second,
			Factor:   2.0,
			Jitter:   0.1,
			Steps:    2,
		}),
	}

	if config != nil && (config.CACertificate != "" || config.Insecure) {
		tlsTransport, err := BuildTLSTransport(config)
		if err != nil {
			return "", err
		}
		defer tlsTransport.CloseIdleConnections()

		options = append(options, remote.WithTransport(tlsTransport))
	}

	desc, err := remote.Get(tag, options...)
	if err != nil {
		var transportErr *transport.Error
		if errors.As(err, &transportErr) && transportErr.StatusCode == http.StatusNotFound {
			return "", &TerminalError{
				Reason:  helmv1alpha1.ReasonSourceNotFound,
				Message: fmt.Sprintf("the repository index offers %s, but the registry has no such tag", ref),
				Err:     err,
			}
		}

		return "", fmt.Errorf("reading the manifest of %s: %w", ref, err)
	}

	manifest, err := v1.ParseManifest(bytes.NewReader(desc.Manifest))
	if err != nil {
		// An unreadable manifest is a verdict about the artifact, not a transport problem.
		return "", &TerminalError{
			Reason:  helmv1alpha1.ReasonUnsupportedChartArtifact,
			Message: "cannot parse the manifest: " + err.Error(),
			Err:     err,
		}
	}

	verdict := examineManifest(desc.MediaType, manifest)
	if !verdict.OK() {
		return "", &TerminalError{
			Reason:  helmv1alpha1.ReasonUnsupportedChartArtifact,
			Message: verdict.Message,
		}
	}

	return verdict.MediaType, nil
}
