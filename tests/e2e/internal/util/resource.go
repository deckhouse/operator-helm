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

package util

import (
	"context"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/deckhouse/operator-helm/tests/e2e/internal/framework"
)

// UntilObjectPhase waits for all objects to reach the expected status.phase.
func UntilObjectPhase(expectedPhase string, timeout time.Duration, objs ...client.Object) {
	GinkgoHelper()
	untilObjectField("status.phase", expectedPhase, timeout, objs...)
}

// UntilObjectState waits for all objects to reach the expected status.state.
func UntilObjectState(expectedState string, timeout time.Duration, objs ...client.Object) {
	GinkgoHelper()
	untilObjectField("status.state", expectedState, timeout, objs...)
}

// UntilConditionTrue waits for the specified condition type to become True
// on all provided objects.
func UntilConditionTrue(conditionType string, timeout time.Duration, objs ...client.Object) {
	GinkgoHelper()
	UntilConditionStatus(conditionType, string(metav1.ConditionTrue), timeout, objs...)
}

// UntilConditionStatus waits for the specified condition to reach the given status.
func UntilConditionStatus(conditionType, expectedStatus string, timeout time.Duration, objs ...client.Object) {
	GinkgoHelper()

	Eventually(func(g Gomega) {
		for _, obj := range objs {
			u := toUnstructured(obj)
			err := framework.GetClients().GenericClient().Get(
				context.Background(), client.ObjectKeyFromObject(obj), u,
			)
			g.Expect(err).NotTo(HaveOccurred())

			conditions, found, err := unstructured.NestedSlice(u.Object, "status", "conditions")
			g.Expect(err).NotTo(HaveOccurred(),
				"failed to access status.conditions of %s", u.GetName())
			g.Expect(found).To(BeTrue(),
				"no status.conditions found on %s", u.GetName())

			var matched bool
			for _, c := range conditions {
				m, ok := c.(map[string]interface{})
				if !ok {
					continue
				}
				if t, _ := m["type"].(string); t == conditionType {
					if observedGeneration, ok := m["observedGeneration"].(int64); ok {
						g.Expect(observedGeneration).To(BeNumerically("==", obj.GetGeneration()))
					}
					status, _ := m["status"].(string)
					g.Expect(status).To(Equal(expectedStatus),
						"object %s condition %s status is %q, expected %q",
						u.GetName(), conditionType, status, expectedStatus)
					matched = true
					break
				}
			}
			g.Expect(matched).To(BeTrue(),
				"condition %s not found on %s", conditionType, u.GetName())
		}
	}).WithTimeout(timeout).WithPolling(framework.PollingInterval).Should(Succeed())
}

// UntilConditionStatus waits for the specified condition to reach the given status considering contdition's LastTranstionTime attribute.
func UntilConditionStatusWithLastTransitionTime(conditionType, expectedStatus string, deployTime metav1.Time, timeout time.Duration, objs ...client.Object) {
	GinkgoHelper()

	Eventually(func(g Gomega) {
		for _, obj := range objs {
			u := toUnstructured(obj)
			err := framework.GetClients().GenericClient().Get(
				context.Background(), client.ObjectKeyFromObject(obj), u,
			)
			g.Expect(err).NotTo(HaveOccurred())

			conditions, found, err := unstructured.NestedSlice(u.Object, "status", "conditions")
			g.Expect(err).NotTo(HaveOccurred(),
				"failed to access status.conditions of %s", u.GetName())
			g.Expect(found).To(BeTrue(),
				"no status.conditions found on %s", u.GetName())

			var matched bool
			for _, c := range conditions {
				m, ok := c.(map[string]interface{})
				if !ok {
					continue
				}
				if t, _ := m["type"].(string); t == conditionType {
					if observedGeneration, ok := m["observedGeneration"].(int64); ok {
						g.Expect(observedGeneration).To(BeNumerically("==", obj.GetGeneration()))
					}
					status, _ := m["status"].(string)
					g.Expect(status).To(Equal(expectedStatus),
						"object %s condition %s status is %q, expected %q",
						u.GetName(), conditionType, status, expectedStatus)
					lastTransitionTimeStr, ok := m["lastTransitionTime"].(string)
					g.Expect(ok).To(BeTrue(), "condition should have lastTransitionTime attribute")
					lastTransitionTime, err := time.Parse(time.RFC3339, lastTransitionTimeStr)
					g.Expect(err).NotTo(HaveOccurred(), "condition should have valid lastTransitionTime value")
					g.Expect(lastTransitionTime.After(deployTime.UTC().Add(-1*time.Second))).To(BeTrue(), "condition has last transition time at %v, which is before deployTime %v", lastTransitionTime, deployTime.UTC())
					matched = true
					break
				}
			}
			g.Expect(matched).To(BeTrue(),
				"condition %s not found on %s", conditionType, u.GetName())
		}
	}).WithTimeout(timeout).WithPolling(framework.PollingInterval).Should(Succeed())
}

// UntilConditionReason waits for the specified condition to have the expected reason.
func UntilConditionReason(conditionType, expectedReason string, timeout time.Duration, objs ...client.Object) {
	GinkgoHelper()
	Eventually(func(g Gomega) {
		for _, obj := range objs {
			u := toUnstructured(obj)
			err := framework.GetClients().GenericClient().Get(
				context.Background(), client.ObjectKeyFromObject(obj), u,
			)
			g.Expect(err).NotTo(HaveOccurred())

			conditions, found, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
			g.Expect(found).To(BeTrue())

			var matched bool
			for _, c := range conditions {
				m, ok := c.(map[string]interface{})
				if !ok {
					continue
				}
				if t, _ := m["type"].(string); t == conditionType {
					reason, _ := m["reason"].(string)
					g.Expect(reason).To(Equal(expectedReason),
						"object %s condition %s reason is %q, expected %q",
						u.GetName(), conditionType, reason, expectedReason)
					matched = true
					break
				}
			}
			g.Expect(matched).To(BeTrue(),
				"condition %s not found on %s", conditionType, u.GetName())
		}
	}).WithTimeout(timeout).WithPolling(framework.PollingInterval).Should(Succeed())
}

// UntilConditionAbsent waits until the specified condition is not present on the
// objects. Abnormal-true conditions are removed rather than set to False, so
// absence is the assertion that matters.
func UntilConditionAbsent(conditionType string, timeout time.Duration, objs ...client.Object) {
	GinkgoHelper()

	Eventually(func(g Gomega) {
		for _, obj := range objs {
			u := toUnstructured(obj)
			err := framework.GetClients().GenericClient().Get(
				context.Background(), client.ObjectKeyFromObject(obj), u,
			)
			g.Expect(err).NotTo(HaveOccurred())

			conditions, _, err := unstructured.NestedSlice(u.Object, "status", "conditions")
			g.Expect(err).NotTo(HaveOccurred(),
				"failed to access status.conditions of %s", u.GetName())

			for _, c := range conditions {
				m, ok := c.(map[string]interface{})
				if !ok {
					continue
				}
				t, _ := m["type"].(string)
				g.Expect(t).NotTo(Equal(conditionType),
					"condition %s must be absent on %s", conditionType, u.GetName())
			}
		}
	}).WithTimeout(timeout).WithPolling(framework.PollingInterval).Should(Succeed())
}

func untilObjectField(fieldPath, expected string, timeout time.Duration, objs ...client.Object) {
	GinkgoHelper()
	Eventually(func(g Gomega) {
		for _, obj := range objs {
			u := toUnstructured(obj)
			err := framework.GetClients().GenericClient().Get(
				context.Background(), client.ObjectKeyFromObject(obj), u,
			)
			g.Expect(err).NotTo(HaveOccurred(),
				"failed to get %s", obj.GetName())

			path := strings.Split(fieldPath, ".")
			value, found, _ := unstructured.NestedString(u.Object, path...)
			actual := "Unknown"
			if found {
				actual = value
			}
			g.Expect(actual).To(Equal(expected),
				"object %s %s is %q, expected %q",
				u.GetName(), fieldPath, actual, expected)
		}
	}).WithTimeout(timeout).WithPolling(framework.PollingInterval).Should(Succeed())
}

func toUnstructured(obj client.Object) *unstructured.Unstructured {
	if u, ok := obj.(*unstructured.Unstructured); ok {
		return u.DeepCopy()
	}

	objMap, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
	Expect(err).NotTo(HaveOccurred(), "failed to convert object to unstructured")
	u := &unstructured.Unstructured{Object: objMap}

	c := framework.GetClients().GenericClient()
	gvks, _, err := c.Scheme().ObjectKinds(obj)
	if err == nil && len(gvks) > 0 {
		u.SetGroupVersionKind(gvks[0])
	} else {
		u.SetGroupVersionKind(schema.GroupVersionKind{})
	}

	return u
}
