/*
Copyright 2026 Flant JSC

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

package rewriter

import (
	"net/http"
	"net/url"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestShouldRewriteAnyResource(t *testing.T) {
	tests := []struct {
		name    string
		path    string
		rewrite bool
	}{
		{
			"crd without rule by name",
			"/apis/apiextensions.k8s.io/v1/customresourcedefinitions/gadgets.example.flant.com",
			true,
		},
		{
			"crd list",
			"/apis/apiextensions.k8s.io/v1/customresourcedefinitions",
			true,
		},
		{
			"resource without rule",
			"/apis/networking.k8s.io/v1/namespaces/default/ingresses/web",
			true,
		},
		{
			"custom resource without rule",
			"/apis/example.flant.com/v1/namespaces/default/gadgets/one",
			true,
		},
		{
			"core resource without rule",
			"/api/v1/namespaces/default/endpoints/web",
			true,
		},
		{
			"status subresource without rule",
			"/apis/example.flant.com/v1/namespaces/default/gadgets/one/status",
			true,
		},
		{
			"resource with rule",
			"/apis/original.group.io/v1/namespaces/default/someresources/one",
			true,
		},
		{
			"apis discovery",
			"/apis",
			true,
		},
		{
			"group discovery without rule",
			"/apis/networking.k8s.io/v1",
			true,
		},
		{
			"core discovery",
			"/api/v1",
			true,
		},
		{
			"openapi v2",
			"/openapi/v2",
			false,
		},
		{
			"metrics",
			"/metrics",
			false,
		},
		{
			"root",
			"/",
			false,
		},
		{
			"unknown",
			"/healthz",
			false,
		},
	}

	rwr := createTestRewriter()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u, err := url.Parse("https://apiserver" + tt.path + "?fieldManager=helm-controller&force=true")
			require.NoError(t, err)
			targetReq := NewTargetRequest(rwr, &http.Request{Method: http.MethodPatch, URL: u})
			require.NotNil(t, targetReq)
			require.Equal(t, tt.rewrite, targetReq.ShouldRewriteRequest(), "request")
			require.Equal(t, tt.rewrite, targetReq.ShouldRewriteResponse(), "response")
		})
	}
}
