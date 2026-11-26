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
	"slices"

	batchv1 "k8s.io/api/batch/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	addonsv1alpha1 "github.com/colonel-byte/cluster-api-addon-provider-zarf/api/v1alpha1"
	"github.com/colonel-byte/cluster-api-addon-provider-zarf/internal/resolver"
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

	// Resolver pins package tags to digests and verifies signatures (§6 step 4). An interface
	// so envtest can substitute a stub - a real resolution needs a registry.
	Resolver resolver.Interface

	// RegistryPlainHTTP speaks HTTP to the package registry, from --registry-plain-http.
	// Test-topology escape hatch, never the default.
	RegistryPlainHTTP bool

	// Recorder emits the condition-transition Events patchStatus publishes. Nil is allowed and
	// emits nothing, which is what most unit and envtest cases want.
	Recorder record.EventRecorder
}

// +kubebuilder:rbac:groups=addons.colonel-byte.dev,resources=zarfpackageproxies,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=addons.colonel-byte.dev,resources=zarfpackageproxies/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=addons.colonel-byte.dev,resources=zarfpackagereleases,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=addons.colonel-byte.dev,resources=zarfpackagereleases/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=cluster.x-k8s.io,resources=clusters,verbs=get;list;watch
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile matches the proxy against clusters and converges its releases.
func (r *ZarfPackageProxyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := ctrl.LoggerFrom(ctx)

	proxy := &addonsv1alpha1.ZarfPackageProxy{}
	if err := r.Get(ctx, req.NamespacedName, proxy); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// A deleting proxy only winds down: no resolution, no stamping - §8B's wait for its
	// releases to finalize, then letting the proxy go.
	if !proxy.DeletionTimestamp.IsZero() {
		retry, err := r.finalizeProxy(ctx, proxy)
		return ctrl.Result{RequeueAfter: retry}, err
	}
	if controllerutil.AddFinalizer(proxy, addonsv1alpha1.FinalizerReleaseCleanup) {
		if err := r.Update(ctx, proxy); err != nil {
			return ctrl.Result{}, err
		}
	}

	matched, err := r.matchClusters(ctx, proxy)
	if err != nil {
		return ctrl.Result{}, err
	}
	log.Info("reconciling", "strategy", proxy.Spec.Strategy, "version", proxy.Spec.Package.Version,
		"matchingClusters", len(matched))

	// §8B runs regardless of cycles or resolution state: deletion must never block on either.
	removalRetry, err := r.finalizeReleases(ctx, proxy)
	if err != nil {
		return ctrl.Result{}, err
	}

	before := proxy.DeepCopy()
	requeueAfter, err := r.resolvePackage(ctx, proxy)
	if err != nil {
		return ctrl.Result{}, err
	}
	if err := r.resolveInitPackage(ctx, proxy); err != nil {
		return ctrl.Result{}, err
	}
	if removalRetry > 0 && (requeueAfter == 0 || removalRetry < requeueAfter) {
		requeueAfter = removalRetry
	}

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
		if err := r.writeDeploySpecs(ctx, proxy, matched); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.convergeDeploys(ctx, proxy, matched); err != nil {
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
	proxy.Status.MatchingClusters = names
	proxy.Status.ObservedGeneration = proxy.Generation
	if len(proxy.Spec.DependsOn) > 0 {
		meta.SetStatusCondition(&proxy.Status.Conditions, validCondition)
	} else {
		meta.RemoveStatusCondition(&proxy.Status.Conditions, addonsv1alpha1.ConditionDependenciesValid)
	}
	if err := r.patchStatus(ctx, proxy, before); err != nil {
		return ctrl.Result{}, err
	}
	// Non-zero only under the follow-tag annotation: the timer that re-resolves the tag.
	return ctrl.Result{RequeueAfter: requeueAfter}, nil
}

// SetupWithManager registers the reconciler with mgr.
func (r *ZarfPackageProxyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&addonsv1alpha1.ZarfPackageProxy{}).
		// Not Owns: releases carry a Cluster controller-ownerRef, never a proxy one
		// (architecture.md §8B), so an Owns watch would never fire. Release events route
		// to the release's own proxy via spec.packageProxyRef, plus §5B's dependent
		// fan-out: every proxy whose dependsOn names that proxy, so verdicts re-evaluate
		// on phase changes rather than on resync.
		Watches(&addonsv1alpha1.ZarfPackageRelease{}, handler.EnqueueRequestsFromMapFunc(r.releaseToProxies)).
		Watches(&clusterv1.Cluster{}, handler.EnqueueRequestsFromMapFunc(r.clusterToProxies)).
		// Deploy Jobs route to the proxy whose release owns them, so completion is absorbed
		// on the event rather than on resync.
		Watches(&batchv1.Job{}, handler.EnqueueRequestsFromMapFunc(r.jobToProxy)).
		Complete(r)
}

// jobToProxy maps a deploy Job event to the proxy whose release the Job realizes, through the
// release label the Job carries and the release's own packageProxyRef - plus every other proxy
// holding a release on the same cluster.
//
// That fan-out is what keeps the §6 step 6 deploy slot live. The slot is *cluster*-scoped:
// clusterSlotHolder lists every Job labelled with the cluster, whichever proxy stamped it. So a
// release held with WaitingForActiveDeploy is waiting on a Job that belongs to a different
// proxy, and mapping the Job to its own proxy alone leaves nothing to wake the waiter when the
// slot frees - the gate holds without requeueing, by design, because an event is meant to bring
// it back. Without this it never comes, and the blocked release sits Pending against a Job that
// has already finished and been reaped.
func (r *ZarfPackageProxyReconciler) jobToProxy(ctx context.Context, obj client.Object) []reconcile.Request {
	name, ok := obj.GetLabels()[releaseLabel]
	if !ok {
		return nil
	}
	release := &addonsv1alpha1.ZarfPackageRelease{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: obj.GetNamespace(), Name: name}, release); err != nil {
		return nil
	}
	requests := releaseToProxy(ctx, release)

	cluster, ok := obj.GetLabels()[clusterLabel]
	if !ok {
		return requests
	}
	releases := &addonsv1alpha1.ZarfPackageReleaseList{}
	if err := r.List(ctx, releases, client.InNamespace(obj.GetNamespace())); err != nil {
		// The owning proxy still gets its event; a missed wake-up is better than none.
		return requests
	}
	for i := range releases.Items {
		other := &releases.Items[i]
		if other.Name == release.Name || other.Spec.ClusterRef.Name != cluster {
			continue
		}
		for _, request := range releaseToProxy(ctx, other) {
			if !slices.Contains(requests, request) {
				requests = append(requests, request)
			}
		}
	}
	return requests
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

// releaseToProxies is releaseToProxy plus §5B's dependent fan-out: proxies whose dependsOn names
// the release's proxy re-evaluate their verdicts on its phase changes. dependsOn resolves in the
// proxy's own namespace, so dependents live where the proxy does.
func (r *ZarfPackageProxyReconciler) releaseToProxies(ctx context.Context, obj client.Object) []reconcile.Request {
	requests := releaseToProxy(ctx, obj)
	if len(requests) == 0 {
		return nil
	}
	own := requests[0].NamespacedName

	proxies := &addonsv1alpha1.ZarfPackageProxyList{}
	if err := r.List(ctx, proxies, client.InNamespace(own.Namespace)); err != nil {
		return requests
	}
	for _, proxy := range proxies.Items {
		if proxy.Name != own.Name && slices.Contains(proxy.Spec.DependsOn, own.Name) {
			requests = append(requests, reconcile.Request{NamespacedName: types.NamespacedName{
				Namespace: proxy.Namespace,
				Name:      proxy.Name,
			}})
		}
	}
	return requests
}
