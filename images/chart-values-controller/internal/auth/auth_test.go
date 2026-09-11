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
	clientset := fake.NewSimpleClientset()

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

	reviewer := New(clientset.AuthenticationV1().TokenReviews(), clientset.AuthorizationV1().SubjectAccessReviews())

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
