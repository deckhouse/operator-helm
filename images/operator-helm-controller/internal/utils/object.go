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

package utils

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// DeleteIfPresent issues a delete for the object if it is still present and reports whether
// it still exists, which it does while a downstream controller's finalizer is running.
func DeleteIfPresent(ctx context.Context, c client.Client, nn types.NamespacedName, obj client.Object) (exists bool, err error) {
	if err := c.Get(ctx, nn, obj); err != nil {
		return false, client.IgnoreNotFound(err)
	}

	if err := c.Delete(ctx, obj); client.IgnoreNotFound(err) != nil {
		return true, fmt.Errorf("failed to delete resource %s/%s: %w", nn.Namespace, nn.Name, err)
	}

	return true, nil
}
