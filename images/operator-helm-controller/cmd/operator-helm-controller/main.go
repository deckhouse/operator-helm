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

package main

import (
	"flag"
	"os"

	helmv2 "github.com/fluxcd/helm-controller/api/v2"
	sourcev1 "github.com/fluxcd/source-controller/api/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
	"github.com/deckhouse/operator-helm/internal/controller/helmapplication"
	"github.com/deckhouse/operator-helm/internal/controller/helmapplicationrepository"
	"github.com/deckhouse/operator-helm/internal/controller/helmclusteraddon"
	"github.com/deckhouse/operator-helm/internal/controller/helmclusteraddonrepository"
	"github.com/deckhouse/operator-helm/internal/controller/helmclusterapplicationrepository"
	"github.com/deckhouse/operator-helm/internal/index"
	helmapplicationwebhook "github.com/deckhouse/operator-helm/internal/webhook/helmapplication"
	helmclusteraddonwebhook "github.com/deckhouse/operator-helm/internal/webhook/helmclusteraddon"
)

var scheme = runtime.NewScheme()

// managedByOperatorHelm selects the RBAC objects this module manages in the users'
// namespaces.
var managedByOperatorHelm = labels.SelectorFromSet(labels.Set{
	helmv1alpha1.LabelManagedBy: helmv1alpha1.LabelManagedByValue,
})

func init() {
	_ = clientgoscheme.AddToScheme(scheme)
	_ = helmv1alpha1.AddToScheme(scheme)
	_ = sourcev1.AddToScheme(scheme)
	_ = helmv2.AddToScheme(scheme)
}

func main() {
	var (
		metricsAddr          string
		healthProbeAddr      string
		enableLeaderElection bool
	)

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metrics endpoint binds to.")
	flag.StringVar(&healthProbeAddr, "health-probe-bind-address", ":9440", "The address the health probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false, "Enable leader election for controller manager.")
	opts := zap.Options{Development: false}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	logger := ctrl.Log.WithName("setup")

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme: scheme,
		Metrics: metricsserver.Options{
			BindAddress: metricsAddr,
		},
		HealthProbeBindAddress: healthProbeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "operator-helm-controller.helm.deckhouse.io",
		Client: client.Options{
			// RBACService reads these three kinds only to reconcile the objects its
			// own release names. The Roles and RoleBindings it manages are watched, but
			// through an informer that selects on the managed-by label — an object
			// stripped of the label is missing from it, and reading through it would
			// then report an object that exists as absent and try to create it again.
			// Reads go to the API server for that reason, and for ServiceAccounts
			// because nothing watches them at all and a cached typed Get would start a
			// cluster-wide informer for the kind.
			Cache: &client.CacheOptions{
				DisableFor: []client.Object{&corev1.ServiceAccount{}, &rbacv1.Role{}, &rbacv1.RoleBinding{}},
			},
		},
		Cache: cache.Options{
			ByObject: map[client.Object]cache.ByObject{
				// The repository controllers watch Secrets, and every Secret they read
				// or write lives in the module namespace, so the informer is scoped
				// there instead of holding every Secret in the cluster in memory.
				&corev1.Secret{}: {
					Namespaces: map[string]cache.Config{
						helmv1alpha1.TargetNamespace: {},
					},
				},
				// The application controller watches the Roles and RoleBindings making
				// up a release identity, in whichever namespace the application lives,
				// so neither informer can be scoped by namespace. The label is what
				// keeps them off every other Role and RoleBinding in the cluster.
				&rbacv1.Role{}:        {Label: managedByOperatorHelm},
				&rbacv1.RoleBinding{}: {Label: managedByOperatorHelm},
			},
		},
	})
	if err != nil {
		logger.Error(err, "unable to create manager")
		os.Exit(1)
	}

	if err := index.SetupAddonRepository(mgr); err != nil {
		logger.Error(err, "unable to setup indexes", "index", index.AddonRepository)
		os.Exit(1)
	}

	if err := index.SetupApplicationRepository(mgr); err != nil {
		logger.Error(err, "unable to setup indexes", "index", index.ApplicationRepository)
		os.Exit(1)
	}

	if err := index.SetupApplicationChart(mgr); err != nil {
		logger.Error(err, "unable to setup indexes", "index", index.ApplicationChart)
		os.Exit(1)
	}

	if err := helmclusteraddonrepository.SetupWithManager(mgr); err != nil {
		logger.Error(err, "unable to setup HelmClusterAddonRepository controller")
		os.Exit(1)
	}

	if err := helmapplicationrepository.SetupWithManager(mgr); err != nil {
		logger.Error(err, "unable to setup HelmApplicationRepository controller")
		os.Exit(1)
	}

	if err := helmclusterapplicationrepository.SetupWithManager(mgr); err != nil {
		logger.Error(err, "unable to setup HelmClusterApplicationRepository controller")
		os.Exit(1)
	}

	if err := helmapplication.SetupWithManager(mgr); err != nil {
		logger.Error(err, "unable to setup HelmApplication controller")
		os.Exit(1)
	}

	if err := helmclusteraddon.SetupWithManager(mgr); err != nil {
		logger.Error(err, "unable to setup HelmClusterAddon controller")
		os.Exit(1)
	}

	if err = index.SetupAddonChart(mgr); err != nil {
		logger.Error(err, "unable to setup indexes", "index", index.AddonChart)
		os.Exit(1)
	}

	if err = helmclusteraddonwebhook.SetupWebhookWithManager(mgr); err != nil {
		logger.Error(err, "unable to create webhook", "webhook", "HelmClusterAddon")
		os.Exit(1)
	}

	if err = helmapplicationwebhook.SetupWebhookWithManager(mgr); err != nil {
		logger.Error(err, "unable to create webhook", "webhook", "HelmApplication")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		logger.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		logger.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	logger.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		logger.Error(err, "manager exited with error")
		os.Exit(1)
	}
}
