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

package internalobj

import (
	"time"

	"github.com/fluxcd/pkg/apis/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func setReconcileRequestAnnotations(obj metav1.Object) {
	annotations := obj.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}

	// One timestamp for both: ForceRequestAnnotation is honoured only when it matches the other.
	ts := time.Now().UTC().Format(time.RFC3339)
	annotations[meta.ForceRequestAnnotation] = ts
	annotations[meta.ReconcileRequestAnnotation] = ts

	obj.SetAnnotations(annotations)
}
