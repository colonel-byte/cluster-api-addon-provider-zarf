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
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	addonsv1alpha1 "github.com/colonel-byte/cluster-api-addon-provider-zarf/api/v1alpha1"
)

// ZarfPackageProxyReconciler reconciles a ZarfPackageProxy: it matches the proxy's selector
// against CAPI Clusters and stamps one ZarfPackageRelease per match.
type ZarfPackageProxyReconciler struct {
	client.Client

	// CachePath is the mount path of the zarf-cache volume inside deploy pods - where each
	// deploy Job caches its pulled package. The controller holds no cache of its own; sizing
	// is a deploy-pod volume concern - see AGENTS.md's quirks section.
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
// shortcut: matching, stamping, and dependency verdicts - resolution and deployment land with
// M2/M3.
func (r *ZarfPackageProxyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := ctrl.LoggerFrom(ctx)

	proxy := &addonsv1alpha1.ZarfPackageProxy{}
	if err := r.Get(ctx, req.NamespacedName, proxy); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	matched, err := r.matchClusters(ctx, proxy)
	if err != nil {
		return ctrl.Result{}, err
	}
	log.Info("reconciling", "strategy", proxy.Spec.Strategy, "version", proxy.Spec.Package.Version,
		"matchingClusters", len(matched))

	inCycle, err := r.inDependencyCycle(ctx, proxy)
	if err != nil {
		return ctrl.Result{}, err
	}
	validCondition := metav1.Condition{
		Type:               addonsv1alpha1.ConditionDependenciesValid,
		Status:             metav1.ConditionTrue,
		Reason:             addonsv1alpha1.ReasonDependenciesValid,
		Message:            "dependsOn forms no cycle",
		ObservedGeneration: proxy.Generation,
	}
	if inCycle {
		validCondition.Status = metav1.ConditionFalse
		validCondition.Reason = addonsv1alpha1.ReasonDependencyCycle
		validCondition.Message = "this proxy is part of a dependsOn cycle; it deploys nothing until the cycle is broken"
	}

	// §5C: a cycle member deploys nothing - no stamping, no verdicts - until the cycle breaks.
	if !inCycle {
		if err := r.stampReleases(ctx, proxy, matched); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.applyDependencyVerdicts(ctx, proxy, matched); err != nil {
			return ctrl.Result{}, err
		}
	}

	// status.matchingClusters names match the selector's reach: bare in the proxy's own
	// namespace, namespace-qualified where --cluster-namespaces let the selector cross.
	names := make([]string, 0, len(matched))
	for _, cluster := range matched {
		name := cluster.Name
		if cluster.Namespace != proxy.Namespace {
			name = cluster.Namespace + "/" + cluster.Name
		}
		names = append(names, name)
	}
	before := proxy.DeepCopy()
	proxy.Status.MatchingClusters = names
	proxy.Status.ObservedGeneration = proxy.Generation
	if len(proxy.Spec.DependsOn) > 0 {
		meta.SetStatusCondition(&proxy.Status.Conditions, validCondition)
	} else {
		meta.RemoveStatusCondition(&proxy.Status.Conditions, addonsv1alpha1.ConditionDependenciesValid)
	}
	if err := r.Status().Patch(ctx, proxy, client.MergeFrom(before)); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// SetupWithManager registers the reconciler with mgr.
func (r *ZarfPackageProxyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&addonsv1alpha1.ZarfPackageProxy{}).
		// Not Owns: releases carry a Cluster controller-ownerRef, never a proxy one
		// (architecture.md §8B), so an Owns watch would never fire. Release events route
		// to the release's own proxy via spec.packageProxyRef. §5B's dependent fan-out -
		// requeueing proxies whose dependsOn names this release's proxy - is a second
		// mapping that does not exist yet; until it does, dependents re-evaluate on resync.
		Watches(&addonsv1alpha1.ZarfPackageRelease{}, handler.EnqueueRequestsFromMapFunc(releaseToProxy)).
		Watches(&clusterv1.Cluster{}, handler.EnqueueRequestsFromMapFunc(r.clusterToProxies)).
		Complete(r)
}

// releaseToProxy maps a release event to the proxy its packageProxyRef names, in the release's
// namespace when the ref's namespace is empty. Routing only - the CrossNamespaceNotEnabled
// gating happens in the reconciler, not here.
func releaseToProxy(_ context.Context, obj client.Object) []reconcile.Request {
	release, ok := obj.(*addonsv1alpha1.ZarfPackageRelease)
	if !ok {
		return nil
	}
	namespace := release.Spec.PackageProxyRef.Namespace
	if namespace == "" {
		namespace = release.Namespace
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{
		Namespace: namespace,
		Name:      release.Spec.PackageProxyRef.Name,
	}}}
}
