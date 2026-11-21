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

// Package controller holds the CAAPZ reconcilers.
package controller

import (
	"context"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	addonsv1alpha1 "github.com/colonel-byte/cluster-api-addon-provider-zarf/api/v1alpha1"
)

// ZarfPackageProxyReconciler reconciles a ZarfPackageProxy: it matches the proxy's selector
// against CAPI Clusters and stamps one ZarfPackageRelease per match.
type ZarfPackageProxyReconciler struct {
	client.Client

	// CachePath is the directory pulled Zarf packages are cached in. It must sit on a mount
	// sized for the largest package plus its extraction - see AGENTS.md's quirks section.
	CachePath string

	// DeployImage is the image deploy Jobs run when a proxy does not override it - normally
	// this controller's own image, which carries the deploy subcommand.
	DeployImage string

	// ClusterNamespaces are the namespaces other than a proxy's own it may select Clusters in;
	// "*" allows any. Empty refuses every cross-namespace selection, surfaced as a condition
	// with reason CrossNamespaceNotEnabled. See docs/dev/architecture.md §2C.
	ClusterNamespaces []string

	// SecretNamespaces are the namespaces other than a proxy's own it may name a secret in;
	// same semantics as ClusterNamespaces, mirroring cargoship's --secret-namespaces.
	SecretNamespaces []string
}

// +kubebuilder:rbac:groups=addons.colonel-byte.dev,resources=zarfpackageproxies,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=addons.colonel-byte.dev,resources=zarfpackageproxies/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=addons.colonel-byte.dev,resources=zarfpackagereleases,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=addons.colonel-byte.dev,resources=zarfpackagereleases/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=cluster.x-k8s.io,resources=clusters,verbs=get;list;watch
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

// Reconcile matches the proxy against clusters and converges its releases.
//
// shortcut: scaffold only - it fetches the proxy and stops; cluster matching, release stamping,
// and deployment land with the first functional milestone.
func (r *ZarfPackageProxyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := ctrl.LoggerFrom(ctx)

	proxy := &addonsv1alpha1.ZarfPackageProxy{}
	if err := r.Get(ctx, req.NamespacedName, proxy); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	log.Info("reconciling", "strategy", proxy.Spec.Strategy, "version", proxy.Spec.Package.Version)
	return ctrl.Result{}, nil
}

// SetupWithManager registers the reconciler with mgr.
func (r *ZarfPackageProxyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&addonsv1alpha1.ZarfPackageProxy{}).
		Owns(&addonsv1alpha1.ZarfPackageRelease{}).
		Complete(r)
}
