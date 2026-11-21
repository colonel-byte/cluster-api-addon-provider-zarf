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

package controller

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	addonsv1alpha1 "github.com/colonel-byte/cluster-api-addon-provider-zarf/api/v1alpha1"
)

// stampReleases converges the proxy's releases onto its matched clusters: one release per
// stampable cluster, in the cluster's namespace, controller-owned by the Cluster (never the
// proxy - §8B: GC deletes a dependent only when all owners are gone, so a second owner would
// stop Cluster deletion from collecting releases). Releases whose cluster no longer matches are
// deleted; §8's finalizer machinery decides uninstall-vs-orphan when it lands (M4) - until then
// deletion simply orphans the deployed package.
func (r *ZarfPackageProxyReconciler) stampReleases(ctx context.Context, proxy *addonsv1alpha1.ZarfPackageProxy, matched []clusterv1.Cluster) error {
	log := ctrl.LoggerFrom(ctx)

	// Existing releases realizing this proxy, keyed by cluster namespace/name. Found by their
	// spec, not by a name convention, so a hand-renamed release still counts (state-keyed).
	releases := &addonsv1alpha1.ZarfPackageReleaseList{}
	if err := r.List(ctx, releases); err != nil {
		return err
	}
	existing := map[string]*addonsv1alpha1.ZarfPackageRelease{}
	for i := range releases.Items {
		release := &releases.Items[i]
		refNS := release.Spec.PackageProxyRef.Namespace
		if refNS == "" {
			refNS = release.Namespace
		}
		if release.Spec.PackageProxyRef.Name != proxy.Name || refNS != proxy.Namespace {
			continue
		}
		existing[release.Namespace+"/"+release.Spec.ClusterRef.Name] = release
	}

	for _, cluster := range matched {
		if !cluster.DeletionTimestamp.IsZero() {
			// §7A: never act toward a deleting cluster; its releases garbage-collect
			// through the Cluster ownerReference.
			delete(existing, cluster.Namespace+"/"+cluster.Name)
			continue
		}
		if !ptr.Deref(cluster.Status.Initialization.ControlPlaneInitialized, false) {
			// §6 step 1: stamp only once the control plane is initialized. The Cluster
			// watch re-enqueues this proxy when the status flips.
			delete(existing, cluster.Namespace+"/"+cluster.Name)
			continue
		}
		if _, ok := existing[cluster.Namespace+"/"+cluster.Name]; ok {
			delete(existing, cluster.Namespace+"/"+cluster.Name)
			continue
		}

		release := &addonsv1alpha1.ZarfPackageRelease{
			ObjectMeta: metav1.ObjectMeta{
				// shortcut: <proxy>-<cluster> is not hash-truncated and can collide
				// ("a-b"+"c" vs "a"+"b-c"); adoption is spec-keyed so only creation
				// cares - add hashing when a fleet hits a 253-byte or colliding name.
				Name:      proxy.Name + "-" + cluster.Name,
				Namespace: cluster.Namespace,
			},
			Spec: addonsv1alpha1.ZarfPackageReleaseSpec{
				ClusterRef: corev1.LocalObjectReference{Name: cluster.Name},
				PackageProxyRef: addonsv1alpha1.NamespacedReference{
					Name: proxy.Name,
				},
			},
		}
		if cluster.Namespace != proxy.Namespace {
			release.Spec.PackageProxyRef.Namespace = proxy.Namespace
		}
		if err := controllerutil.SetControllerReference(&cluster, release, r.Scheme()); err != nil {
			return err
		}
		if err := r.Create(ctx, release); err != nil {
			if apierrors.IsAlreadyExists(err) {
				// A release by this name realizes a different proxy or cluster; stamping
				// it would hijack it. Surface rather than overwrite.
				log.Info("release name already taken", "release", client.ObjectKeyFromObject(release))
				continue
			}
			return err
		}
		before := release.DeepCopy()
		release.Status.Phase = addonsv1alpha1.PhasePending
		if err := r.Status().Patch(ctx, release, client.MergeFrom(before)); err != nil {
			return err
		}
		log.Info("stamped release", "release", client.ObjectKeyFromObject(release), "cluster", cluster.Name)
	}

	// Whatever is left no longer corresponds to a matched, stampable cluster. Deleting-cluster
	// and uninitialized entries were removed above, so this is the un-match path only.
	for _, release := range existing {
		if !release.DeletionTimestamp.IsZero() {
			continue
		}
		if err := r.Delete(ctx, release); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		log.Info("deleted un-matched release", "release", client.ObjectKeyFromObject(release))
	}
	return nil
}
