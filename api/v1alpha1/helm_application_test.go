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

package v1alpha1

import (
	"slices"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestHelmApplicationRepositoryName(t *testing.T) {
	cases := []struct {
		name string
		ref  HelmApplicationChartRef
		want string
	}{
		{
			name: "a namespaced repository",
			ref:  HelmApplicationChartRef{Repository: "myapp-repo"},
			want: "myapp-repo",
		},
		{
			name: "a cluster repository",
			ref:  HelmApplicationChartRef{ClusterRepository: "shared-repo"},
			want: "shared-repo",
		},
		{
			name: "neither reference set",
			ref:  HelmApplicationChartRef{},
			want: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			app := &HelmApplication{Spec: HelmApplicationSpec{Chart: tc.ref}}

			if got := app.RepositoryName(); got != tc.want {
				t.Fatalf("RepositoryName() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestHelmApplicationRepositoryKind(t *testing.T) {
	cases := []struct {
		name string
		ref  HelmApplicationChartRef
		want string
	}{
		{
			name: "a namespaced repository",
			ref:  HelmApplicationChartRef{Repository: "myapp-repo"},
			want: HelmApplicationRepositoryKind,
		},
		{
			name: "a cluster repository",
			ref:  HelmApplicationChartRef{ClusterRepository: "shared-repo"},
			want: HelmClusterApplicationRepositoryKind,
		},
		{
			name: "neither reference set is reported as unknown, not as a cluster repository",
			ref:  HelmApplicationChartRef{},
			want: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			app := &HelmApplication{Spec: HelmApplicationSpec{Chart: tc.ref}}

			if got := app.RepositoryKind(); got != tc.want {
				t.Fatalf("RepositoryKind() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestHelmApplicationIsChartStatusInfoOutdated(t *testing.T) {
	spec := HelmApplicationChartRef{
		Name:       "nginx",
		Repository: "myapp-repo",
		Version:    "1.0.0",
	}

	cases := []struct {
		name             string
		lastAppliedChart *HelmApplicationLastAppliedChartRef
		want             bool
	}{
		{
			name:             "nothing applied yet",
			lastAppliedChart: nil,
			want:             true,
		},
		{
			name: "the applied chart matches the spec",
			lastAppliedChart: &HelmApplicationLastAppliedChartRef{
				Name:       "nginx",
				Repository: "myapp-repo",
				Version:    "1.0.0",
			},
			want: false,
		},
		{
			name: "the version changed",
			lastAppliedChart: &HelmApplicationLastAppliedChartRef{
				Name:       "nginx",
				Repository: "myapp-repo",
				Version:    "0.9.0",
			},
			want: true,
		},
		{
			name: "the same repository name, but it is now a cluster repository",
			lastAppliedChart: &HelmApplicationLastAppliedChartRef{
				Name:              "nginx",
				ClusterRepository: "myapp-repo",
				Version:           "1.0.0",
			},
			want: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			app := &HelmApplication{
				Spec:   HelmApplicationSpec{Chart: spec},
				Status: HelmApplicationStatus{LastAppliedChart: tc.lastAppliedChart},
			}

			if got := app.IsChartStatusInfoOutdated(); got != tc.want {
				t.Fatalf("IsChartStatusInfoOutdated() = %v, want %v", got, tc.want)
			}
		})
	}

	// The cases above all differ in a field the addon's three-field comparison
	// already covers, so none of them would fail if clusterRepository were dropped
	// from the comparison. This one isolates it: the namespaced reference is unset
	// on both sides, so clusterRepository is the only field left that differs.
	t.Run("only the cluster repository changed, with the namespaced reference unset on both sides", func(t *testing.T) {
		app := &HelmApplication{
			Spec: HelmApplicationSpec{Chart: HelmApplicationChartRef{
				Name:              "nginx",
				ClusterRepository: "shared-a",
				Version:           "1.0.0",
			}},
			Status: HelmApplicationStatus{LastAppliedChart: &HelmApplicationLastAppliedChartRef{
				Name:              "nginx",
				ClusterRepository: "shared-b",
				Version:           "1.0.0",
			}},
		}

		if !app.IsChartStatusInfoOutdated() {
			t.Fatal("IsChartStatusInfoOutdated() = false, want true: only clusterRepository differs")
		}
	})
}

func TestHelmApplicationGetConditionTypesForUpdate(t *testing.T) {
	chart := HelmApplicationChartRef{Name: "nginx", Repository: "myapp-repo", Version: "1.0.0"}
	applied := &HelmApplicationLastAppliedChartRef{Name: "nginx", Repository: "myapp-repo", Version: "1.0.0"}
	installed := []metav1.Condition{{Type: ConditionTypeInstalled, Status: metav1.ConditionTrue}}

	t.Run("nothing installed yet asks only about Installed", func(t *testing.T) {
		app := &HelmApplication{Spec: HelmApplicationSpec{Chart: chart}}

		got := app.GetConditionTypesForUpdate()

		if !slices.Contains(got, ConditionTypeInstalled) {
			t.Fatalf("GetConditionTypesForUpdate() = %v, want it to contain %s", got, ConditionTypeInstalled)
		}
		if slices.Contains(got, ConditionTypeUpdateInstalled) {
			t.Fatalf("GetConditionTypesForUpdate() = %v, want it not to contain %s", got, ConditionTypeUpdateInstalled)
		}
	})

	t.Run("an installed application in sync asks only about Ready", func(t *testing.T) {
		app := &HelmApplication{
			Spec:   HelmApplicationSpec{Chart: chart},
			Status: HelmApplicationStatus{LastAppliedChart: applied, Conditions: installed},
		}

		got := app.GetConditionTypesForUpdate()

		if len(got) != 1 || got[0] != ConditionTypeReady {
			t.Fatalf("GetConditionTypesForUpdate() = %v, want [%s]", got, ConditionTypeReady)
		}
	})

	t.Run("a changed chart version asks about UpdateInstalled", func(t *testing.T) {
		app := &HelmApplication{
			Spec: HelmApplicationSpec{Chart: HelmApplicationChartRef{Name: "nginx", Repository: "myapp-repo", Version: "2.0.0"}},
			Status: HelmApplicationStatus{
				LastAppliedChart: applied,
				Conditions:       installed,
			},
		}

		got := app.GetConditionTypesForUpdate()

		if !slices.Contains(got, ConditionTypeUpdateInstalled) {
			t.Fatalf("GetConditionTypesForUpdate() = %v, want it to contain %s", got, ConditionTypeUpdateInstalled)
		}
	})

	t.Run("changed values ask about ConfigurationApplied", func(t *testing.T) {
		app := &HelmApplication{
			Spec: HelmApplicationSpec{
				Chart:  chart,
				Values: &apiextensionsv1.JSON{Raw: []byte(`{"replicaCount":2}`)},
			},
			Status: HelmApplicationStatus{LastAppliedChart: applied, Conditions: installed},
		}

		got := app.GetConditionTypesForUpdate()

		if !slices.Contains(got, ConditionTypeConfigurationApplied) {
			t.Fatalf("GetConditionTypesForUpdate() = %v, want it to contain %s", got, ConditionTypeConfigurationApplied)
		}
	})
}

func TestHelmApplicationMaintenanceModeActivated(t *testing.T) {
	cases := []struct {
		name        string
		maintenance string
		want        bool
	}{
		{name: "standard reconciliation", maintenance: "", want: false},
		{name: "maintenance requested", maintenance: string(NoResourceReconciliation), want: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			app := &HelmApplication{Spec: HelmApplicationSpec{Maintenance: tc.maintenance}}

			if got := app.MaintenanceModeActivated(); got != tc.want {
				t.Fatalf("MaintenanceModeActivated() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestHelmApplicationMaintenanceModeEnabled(t *testing.T) {
	cases := []struct {
		name       string
		conditions []metav1.Condition
		want       bool
	}{
		{
			name:       "no conditions at all",
			conditions: nil,
			want:       false,
		},
		{
			name:       "still managed",
			conditions: []metav1.Condition{{Type: ConditionTypeManaged, Status: metav1.ConditionTrue}},
			want:       false,
		},
		{
			name:       "no longer managed",
			conditions: []metav1.Condition{{Type: ConditionTypeManaged, Status: metav1.ConditionFalse}},
			want:       true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			app := &HelmApplication{Status: HelmApplicationStatus{Conditions: tc.conditions}}

			if got := app.MaintenanceModeEnabled(); got != tc.want {
				t.Fatalf("MaintenanceModeEnabled() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestHelmApplicationForceReconcileRequired(t *testing.T) {
	cases := []struct {
		name        string
		annotations map[string]string
		want        bool
	}{
		{name: "no annotations at all", annotations: nil, want: false},
		{name: "an unrelated annotation", annotations: map[string]string{"example.com/other": ""}, want: false},
		{name: "the force reconcile annotation with an empty value", annotations: map[string]string{AnnotationForceReconcile: ""}, want: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			app := &HelmApplication{ObjectMeta: metav1.ObjectMeta{Annotations: tc.annotations}}

			if got := app.ForceReconcileRequired(); got != tc.want {
				t.Fatalf("ForceReconcileRequired() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestHelmApplicationGetConditions(t *testing.T) {
	app := &HelmApplication{}

	conditions := app.GetConditions()
	*conditions = append(*conditions, metav1.Condition{Type: ConditionTypeReady, Status: metav1.ConditionTrue})

	if len(app.Status.Conditions) != 1 {
		t.Fatalf("appending through GetConditions did not reach the status: got %d conditions, want 1", len(app.Status.Conditions))
	}
}

func TestHelmApplicationObservedGeneration(t *testing.T) {
	app := &HelmApplication{}

	app.SetObservedGeneration(11)

	if got := app.GetObservedGeneration(); got != 11 {
		t.Fatalf("GetObservedGeneration() = %d, want 11", got)
	}
}
