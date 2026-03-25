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
					observedGeneration, _ := m["observedGeneration"].(int64)
					g.Expect(observedGeneration).To(BeNumerically("==", obj.GetGeneration()))

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
	}).WithTimeout(timeout).WithPolling(time.Second).Should(Succeed())
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
	}).WithTimeout(timeout).WithPolling(time.Second).Should(Succeed())
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
	}).WithTimeout(timeout).WithPolling(time.Second).Should(Succeed())
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
