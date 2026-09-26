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

package auth

import (
	"context"
	"testing"

	authnv1 "k8s.io/api/authentication/v1"
	authzv1 "k8s.io/api/authorization/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// TestReviewCarriesTheNamespaceIntoTheAccessReview pins the check that makes a
// namespaced resource's authorization meaningful: without the namespace the API
// server answers for the cluster scope, which is a different question entirely.
func TestReviewCarriesTheNamespaceIntoTheAccessReview(t *testing.T) {
	clientset := fake.NewClientset()

	clientset.PrependReactor("create", "tokenreviews", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, &authnv1.TokenReview{
			Status: authnv1.TokenReviewStatus{
				Authenticated: true,
				User:          authnv1.UserInfo{Username: "alice", UID: "uid-1", Groups: []string{"dev"}},
			},
		}, nil
	})

	var recorded *authzv1.SubjectAccessReview
	clientset.PrependReactor("create", "subjectaccessreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
		recorded = action.(k8stesting.CreateAction).GetObject().(*authzv1.SubjectAccessReview)

		return true, &authzv1.SubjectAccessReview{Status: authzv1.SubjectAccessReviewStatus{Allowed: true}}, nil
	})

	reviewer := New(clientset.AuthenticationV1().TokenReviews(), clientset.AuthorizationV1().SubjectAccessReviews(), nil)

	result, err := reviewer.Review(context.Background(), "token", Access{
		Group:     "helm.deckhouse.io",
		Resource:  "helmapplications",
		Verb:      "create",
		Namespace: "team-a",
	})
	if err != nil {
		t.Fatalf("Review returned %v", err)
	}
	if !result.Authenticated || !result.Authorized || result.Username != "alice" {
		t.Fatalf("result = %+v", result)
	}

	if recorded == nil {
		t.Fatal("no SubjectAccessReview was created")
	}
	attrs := recorded.Spec.ResourceAttributes
	if attrs == nil || attrs.Namespace != "team-a" || attrs.Resource != "helmapplications" || attrs.Verb != "create" {
		t.Fatalf("resource attributes = %+v, want a namespaced create on helmapplications", attrs)
	}
	if recorded.Spec.User != "alice" || len(recorded.Spec.Groups) != 1 {
		t.Fatalf("subject = %q groups %v, want the reviewed identity", recorded.Spec.User, recorded.Spec.Groups)
	}
}

// reviewerFor wires a Reviewer onto a clientset whose TokenReview answers with
// status, records the review that was sent, and allows every access review, so a
// test only has to say what the API server knows about the token.
func reviewerFor(t *testing.T, audiences []string, status authnv1.TokenReviewStatus) (*Reviewer, **authnv1.TokenReview) {
	t.Helper()

	clientset := fake.NewClientset()
	recorded := new(*authnv1.TokenReview)

	clientset.PrependReactor("create", "tokenreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
		*recorded = action.(k8stesting.CreateAction).GetObject().(*authnv1.TokenReview)

		return true, &authnv1.TokenReview{Status: status}, nil
	})
	clientset.PrependReactor("create", "subjectaccessreviews", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, &authzv1.SubjectAccessReview{Status: authzv1.SubjectAccessReviewStatus{Allowed: true}}, nil
	})

	return New(clientset.AuthenticationV1().TokenReviews(), clientset.AuthorizationV1().SubjectAccessReviews(), audiences), recorded
}

var testAccess = Access{Group: "helm.deckhouse.io", Resource: "helmclusteraddons", Verb: "create"}

// TestReviewAcceptsATokenIssuedForTheConfiguredAudience pins both halves of the
// audience check: the audience is asked for in the TokenReview, and a token the
// API server confirms it for is accepted.
func TestReviewAcceptsATokenIssuedForTheConfiguredAudience(t *testing.T) {
	reviewer, recorded := reviewerFor(t, []string{"chart-values-api"}, authnv1.TokenReviewStatus{
		Authenticated: true,
		Audiences:     []string{"chart-values-api"},
		User:          authnv1.UserInfo{Username: "alice"},
	})

	result, err := reviewer.Review(context.Background(), "token", testAccess)
	if err != nil {
		t.Fatalf("Review returned %v", err)
	}
	if !result.Authenticated || !result.Authorized {
		t.Fatalf("result = %+v, want an authenticated and authorized caller", result)
	}
	if *recorded == nil {
		t.Fatal("no TokenReview was created")
	}
	if got := (*recorded).Spec.Audiences; len(got) != 1 || got[0] != "chart-values-api" {
		t.Fatalf("requested audiences = %v, want the configured one", got)
	}
}

// TestReviewRejectsATokenIssuedForAnotherAudience is the point of the check: a
// token the cluster happily authenticates, but which was minted for a different
// recipient, must not pass as authentication here.
func TestReviewRejectsATokenIssuedForAnotherAudience(t *testing.T) {
	for name, status := range map[string]authnv1.TokenReviewStatus{
		"no audience confirmed": {
			Authenticated: true,
			User:          authnv1.UserInfo{Username: "alice"},
		},
		"another audience confirmed": {
			Authenticated: true,
			Audiences:     []string{"some-other-service"},
			User:          authnv1.UserInfo{Username: "alice"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			reviewer, _ := reviewerFor(t, []string{"chart-values-api"}, status)

			result, err := reviewer.Review(context.Background(), "token", testAccess)
			if err != nil {
				t.Fatalf("Review returned %v", err)
			}
			if result.Authenticated || result.Authorized {
				t.Fatalf("result = %+v, want the token rejected as unauthenticated", result)
			}
		})
	}
}

// TestReviewWithoutAConfiguredAudienceAsksForNone keeps the default behaviour
// explicit: nothing is required of the token beyond what the API server checks
// against its own default audience, which is what clients rely on today.
func TestReviewWithoutAConfiguredAudienceAsksForNone(t *testing.T) {
	reviewer, recorded := reviewerFor(t, nil, authnv1.TokenReviewStatus{
		Authenticated: true,
		Audiences:     []string{"https://kubernetes.default.svc.cluster.local"},
		User:          authnv1.UserInfo{Username: "alice"},
	})

	result, err := reviewer.Review(context.Background(), "token", testAccess)
	if err != nil {
		t.Fatalf("Review returned %v", err)
	}
	if !result.Authenticated || !result.Authorized {
		t.Fatalf("result = %+v, want the token accepted", result)
	}
	if got := (*recorded).Spec.Audiences; len(got) != 0 {
		t.Fatalf("requested audiences = %v, want none", got)
	}
}
