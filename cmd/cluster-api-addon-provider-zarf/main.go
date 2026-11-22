// Copyright 2026 colonel-byte
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Command cluster-api-addon-provider-zarf runs the CAAPZ controller manager in a CAPI management
// cluster. See docs/dev/architecture.md for the design.
package main

import (
	"flag"
	"os"
	"strings"

	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	addonsv1alpha1 "github.com/colonel-byte/cluster-api-addon-provider-zarf/api/v1alpha1"
	"github.com/colonel-byte/cluster-api-addon-provider-zarf/config"
	"github.com/colonel-byte/cluster-api-addon-provider-zarf/internal/controller"
	"github.com/colonel-byte/cluster-api-addon-provider-zarf/internal/resolver"
)

// namespaceListFlag registers a comma-separated namespace list flag and returns the parsed list.
// Whitespace is trimmed and empty entries dropped; nil until flag.Parse runs.
func namespaceListFlag(name, usage string) *[]string {
	list := &[]string{}
	flag.Func(name, usage, func(v string) error {
		*list = nil
		for _, ns := range strings.Split(v, ",") {
			if ns = strings.TrimSpace(ns); ns != "" {
				*list = append(*list, ns)
			}
		}
		return nil
	})
	return list
}

func main() {
	var metricsAddr, probeAddr, cachePath, deployImage string
	var enableLeaderElection bool
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "address the metric endpoint binds to, or 0 to disable")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "address the probe endpoint binds to")
	// Zarf packages are whole-platform OCI artifacts, routinely tens of gigabytes; the volume
	// behind this path in each deploy pod has to be sized for the largest package plus its
	// extraction. The controller itself holds no cache. See "Quirks worth knowing" in AGENTS.md.
	flag.StringVar(&cachePath, "cache-path", "/var/lib/caapz/cache", "mount path of the zarf-cache volume inside deploy pods; back it with a volume sized for the largest package")
	flag.StringVar(&deployImage, "deploy-image", "", "image deploy Jobs run; normally this controller's own image")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false, "run only one active replica")
	var registryPlainHTTP bool
	flag.BoolVar(&registryPlainHTTP, "registry-plain-http", false,
		"speak HTTP to the package registry; for test topologies, never production")
	// Mirrors cargoship's --secret-namespaces design: empty refuses every cross-namespace
	// selection or reference, surfaced as a CrossNamespaceNotEnabled condition.
	clusterNamespaces := namespaceListFlag("cluster-namespaces",
		"comma-separated namespaces a proxy may select Clusters in besides its own, or * for any; empty allows none")
	secretNamespaces := namespaceListFlag("secret-namespaces",
		"comma-separated namespaces a proxy may reference a secret in besides its own, or * for any; empty allows none")
	zapOpts := zap.Options{}
	zapOpts.BindFlags(flag.CommandLine)
	flag.Parse()
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&zapOpts)))

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		ctrl.Log.Error(err, "unable to register the client-go scheme")
		os.Exit(1)
	}
	if err := addonsv1alpha1.AddToScheme(scheme); err != nil {
		ctrl.Log.Error(err, "unable to register the addons scheme")
		os.Exit(1)
	}
	if err := clusterv1.AddToScheme(scheme); err != nil {
		ctrl.Log.Error(err, "unable to register the cluster-api scheme")
		os.Exit(1)
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "caapz.addons.colonel-byte.dev",
		HealthProbeBindAddress: probeAddr,
	})
	if err != nil {
		ctrl.Log.Error(err, "unable to build the manager")
		os.Exit(1)
	}

	reconciler := &controller.ZarfPackageProxyReconciler{
		Client:            mgr.GetClient(),
		CachePath:         cachePath,
		DeployImage:       deployImage,
		ClusterNamespaces: *clusterNamespaces,
		SecretNamespaces:  *secretNamespaces,
		Resolver:          resolver.Resolver{},
		RegistryPlainHTTP: registryPlainHTTP,
	}
	if err := reconciler.SetupWithManager(mgr); err != nil {
		ctrl.Log.Error(err, "unable to set up the ZarfPackageProxy controller")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		ctrl.Log.Error(err, "unable to set up the health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		ctrl.Log.Error(err, "unable to set up the ready check")
		os.Exit(1)
	}

	ctrl.Log.Info("starting the CAAPZ addon provider", "version", config.CLIVersion, "commit", config.CLICommit)
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		ctrl.Log.Error(err, "the manager exited with an error")
		os.Exit(1)
	}
}
