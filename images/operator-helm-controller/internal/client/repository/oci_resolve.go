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

// chartArtifactProbeBudget bounds the whole probe: the two attempts and the pause
// between them. It is a variable only so the test that pins it need not wait it out.
//
// The probe serves one release that is already reconciling, and a stalled registry
// otherwise holds that reconcile open indefinitely — neither the reconcile context nor
// go-containerregistry's transport bounds the wait for a response. Thirty seconds is
// two attempts at a request a healthy registry answers in milliseconds, and it is
// short next to chartArtifactProbeRequeueInterval (2 minutes, see internalobj), which
// is what actually brings the recovering registry back.
var chartArtifactProbeBudget = 30 * time.Second

// ResolveChartArtifact reads one manifest and applies the same "is this a chart"
// rule the catalog client applies, so the two can never disagree about an artifact.
//
// config carries transport settings only: its credentials are deliberately ignored.
// A chart version a helm index points at lives on a registry the repository merely
// names, and the internal OCIRepository is built without credentials for it — a probe
// that authenticated would succeed where the pull it authorizes would fail, which is
// worse than failing here.
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
		// Two attempts: a failure is reported and retried by the next pass anyway,
		// and a longer backoff here would hold the addon's reconciliation open.
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
		// An unreadable manifest is a verdict about the artifact, not a transport
		// problem, so it is reported the same way as an unsupported one.
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
