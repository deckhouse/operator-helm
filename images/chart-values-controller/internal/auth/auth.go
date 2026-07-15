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
	"fmt"

	authnv1 "k8s.io/api/authentication/v1"
	authzv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	authnclientv1 "k8s.io/client-go/kubernetes/typed/authentication/v1"
	authzclientv1 "k8s.io/client-go/kubernetes/typed/authorization/v1"
)

// Access is the cluster-scoped resource permission a request must hold. It maps
// directly onto a SubjectAccessReview resource attribute check.
type Access struct {
	Group    string
	Resource string
	Verb     string
}

// Result reports the outcome of reviewing a bearer token. Authorized is only
// meaningful when Authenticated is true.
type Result struct {
	Authenticated bool
	Authorized    bool
	Username      string
}

// Reviewer authenticates bearer tokens via TokenReview and authorizes the
// resulting subject via SubjectAccessReview.
type Reviewer struct {
	tokenReviews  authnclientv1.TokenReviewInterface
	accessReviews authzclientv1.SubjectAccessReviewInterface
}

func New(tokenReviews authnclientv1.TokenReviewInterface, accessReviews authzclientv1.SubjectAccessReviewInterface) *Reviewer {
	return &Reviewer{tokenReviews: tokenReviews, accessReviews: accessReviews}
}

// Review authenticates the token and, if authentication succeeds, checks whether
// the subject is allowed the given access. A returned error indicates an
// internal failure; expected states are reported via Result.
func (r *Reviewer) Review(ctx context.Context, token string, access Access) (Result, error) {
	reviewed, err := r.tokenReviews.Create(ctx, &authnv1.TokenReview{
		Spec: authnv1.TokenReviewSpec{Token: token},
	}, metav1.CreateOptions{})
	if err != nil {
		return Result{}, fmt.Errorf("creating token review: %w", err)
	}
	if !reviewed.Status.Authenticated {
		return Result{Authenticated: false}, nil
	}

	user := reviewed.Status.User

	allowed, err := r.accessReviews.Create(ctx, &authzv1.SubjectAccessReview{
		Spec: authzv1.SubjectAccessReviewSpec{
			User:   user.Username,
			UID:    user.UID,
			Groups: user.Groups,
			Extra:  convertExtra(user.Extra),
			ResourceAttributes: &authzv1.ResourceAttributes{
				Verb:     access.Verb,
				Group:    access.Group,
				Resource: access.Resource,
			},
		},
	}, metav1.CreateOptions{})
	if err != nil {
		return Result{}, fmt.Errorf("creating subject access review: %w", err)
	}

	return Result{
		Authenticated: true,
		Authorized:    allowed.Status.Allowed,
		Username:      user.Username,
	}, nil
}

func convertExtra(extra map[string]authnv1.ExtraValue) map[string]authzv1.ExtraValue {
	if len(extra) == 0 {
		return nil
	}

	out := make(map[string]authzv1.ExtraValue, len(extra))
	for key, value := range extra {
		out[key] = authzv1.ExtraValue(value)
	}

	return out
}
