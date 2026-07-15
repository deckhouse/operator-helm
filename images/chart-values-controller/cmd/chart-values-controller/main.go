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
	"net/http"
	"os"
	"time"

	sourcev1 "github.com/werf/nelm-source-controller/api/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/deckhouse/chart-values-controller/internal/auth"
	"github.com/deckhouse/chart-values-controller/internal/cache"
	"github.com/deckhouse/chart-values-controller/internal/controller"
	"github.com/deckhouse/chart-values-controller/internal/resolver"
	"github.com/deckhouse/chart-values-controller/internal/server"
	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
)

// artifactDownloadTimeout bounds a single artifact download from the in-cluster
// source-controller.
const artifactDownloadTimeout = 60 * time.Second

var scheme = runtime.NewScheme()

func init() {
	_ = clientgoscheme.AddToScheme(scheme)
	_ = helmv1alpha1.AddToScheme(scheme)
	_ = sourcev1.AddToScheme(scheme)
}

func main() {
	var (
		metricsAddr     string
		healthProbeAddr string
		apiAddr         string
		apiTLSCertFile  string
		apiTLSKeyFile   string
		cacheDir        string
		cacheTTL        time.Duration
		sourceInterval  time.Duration
		maxChartSizeMB  int
	)

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metrics endpoint binds to.")
	flag.StringVar(&healthProbeAddr, "health-probe-bind-address", ":9440", "The address the health probe endpoint binds to.")
	flag.StringVar(&apiAddr, "api-bind-address", "127.0.0.1:8081", "The address the chart-values HTTP API binds to.")
	flag.StringVar(&apiTLSCertFile, "api-tls-cert-file", "", "Path to the PEM-encoded certificate for the chart-values HTTP API; enables TLS together with --api-tls-key-file.")
	flag.StringVar(&apiTLSKeyFile, "api-tls-key-file", "", "Path to the PEM-encoded private key for the chart-values HTTP API; enables TLS together with --api-tls-cert-file.")
	flag.StringVar(&cacheDir, "cache-dir", "/cache", "Directory used to cache extracted values.yaml files.")
	flag.DurationVar(&cacheTTL, "chart-values-cache-ttl", time.Hour, "Lifetime of auxiliary source resources and their cache entries; refreshed on each request.")
	flag.DurationVar(&sourceInterval, "source-interval", 10*time.Minute, "Reconcile interval set on auxiliary source resources.")
	flag.IntVar(&maxChartSizeMB, "max-chart-size-mb", 100, "Maximum accepted chart artifact size in MiB; larger artifacts are rejected.")
	opts := zap.Options{Development: false}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	logger := ctrl.Log.WithName("setup")

	if maxChartSizeMB <= 0 {
		logger.Error(nil, "invalid --max-chart-size-mb, must be greater than 0", "value", maxChartSizeMB)
		os.Exit(1)
	}
	maxArtifactBytes := int64(maxChartSizeMB) << 20

	if (apiTLSCertFile == "") != (apiTLSKeyFile == "") {
		logger.Error(nil, "--api-tls-cert-file and --api-tls-key-file must be set together")
		os.Exit(1)
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme: scheme,
		Metrics: metricsserver.Options{
			BindAddress: metricsAddr,
		},
		HealthProbeBindAddress: healthProbeAddr,
		LeaderElection:         false,
	})
	if err != nil {
		logger.Error(err, "unable to create manager")
		os.Exit(1)
	}

	clientset, err := kubernetes.NewForConfig(mgr.GetConfig())
	if err != nil {
		logger.Error(err, "unable to create kubernetes clientset")
		os.Exit(1)
	}
	reviewer := auth.New(
		clientset.AuthenticationV1().TokenReviews(),
		clientset.AuthorizationV1().SubjectAccessReviews(),
	)

	valuesCache := cache.New(cacheDir)
	httpClient := &http.Client{Timeout: artifactDownloadTimeout}

	res := resolver.New(
		mgr.GetClient(),
		valuesCache,
		httpClient,
		helmv1alpha1.TargetNamespace,
		cacheTTL,
		sourceInterval,
		maxArtifactBytes,
	)

	apiServer := server.New(apiAddr, res, reviewer, server.NewOptions{
		TLSCertFile: apiTLSCertFile,
		TLSKeyFile:  apiTLSKeyFile,
	})
	if err := mgr.Add(apiServer); err != nil {
		logger.Error(err, "unable to add HTTP server")
		os.Exit(1)
	}

	if err := controller.SetupWithManager(mgr, valuesCache, httpClient, maxArtifactBytes); err != nil {
		logger.Error(err, "unable to setup auxiliary-resource controllers")
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
