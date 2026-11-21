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
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	addonsv1alpha1 "github.com/colonel-byte/cluster-api-addon-provider-zarf/api/v1alpha1"
)

// inDependencyCycle reports whether the proxy is a member of a dependsOn cycle, by following
// dependsOn edges through the namespace's proxies (informer cache, no API round trips - §5C) and
// asking whether the proxy can reach itself. Missing proxies are dead ends here, not errors; the
// per-cluster verdict (§5B) is what names them.
func (r *ZarfPackageProxyReconciler) inDependencyCycle(ctx context.Context, proxy *addonsv1alpha1.ZarfPackageProxy) (bool, error) {
	visited := map[string]bool{}
	frontier := append([]string{}, proxy.Spec.DependsOn...)
	for len(frontier) > 0 {
		name := frontier[len(frontier)-1]
		frontier = frontier[:len(frontier)-1]
		if name == proxy.Name {
			return true, nil
		}
		if visited[name] {
			continue
		}
		visited[name] = true

		dep := &addonsv1alpha1.ZarfPackageProxy{}
		err := r.Get(ctx, types.NamespacedName{Namespace: proxy.Namespace, Name: name}, dep)
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return false, err
		}
		frontier = append(frontier, dep.Spec.DependsOn...)
	}
	return false, nil
}

// dependencyVerdict resolves every dependsOn entry for one cluster (§5B) and returns the
// DependenciesSatisfied condition to set on that cluster's release. Entries are checked in
// declared order and the first non-satisfied one decides the condition, its name in the message.
func (r *ZarfPackageProxyReconciler) dependencyVerdict(ctx context.Context, proxy *addonsv1alpha1.ZarfPackageProxy, cluster *clusterv1.Cluster) (metav1.Condition, error) {
	satisfied := metav1.Condition{
		Type:               addonsv1alpha1.ConditionDependenciesSatisfied,
		Status:             metav1.ConditionTrue,
		Reason:             addonsv1alpha1.ReasonDependenciesSatisfied,
		Message:            "every dependency is deployed on this cluster",
		ObservedGeneration: proxy.Generation,
	}
	failed := func(reason, message string) metav1.Condition {
		return metav1.Condition{
			Type:               addonsv1alpha1.ConditionDependenciesSatisfied,
			Status:             metav1.ConditionFalse,
			Reason:             reason,
			Message:            message,
			ObservedGeneration: proxy.Generation,
		}
	}

	for _, name := range proxy.Spec.DependsOn {
		dep := &addonsv1alpha1.ZarfPackageProxy{}
		err := r.Get(ctx, types.NamespacedName{Namespace: proxy.Namespace, Name: name}, dep)
		if apierrors.IsNotFound(err) {
			return failed(addonsv1alpha1.ReasonDependencyNotFound,
				fmt.Sprintf("dependency %q names no ZarfPackageProxy in namespace %s", name, proxy.Namespace)), nil
		}
		if err != nil {
			return metav1.Condition{}, err
		}

		selector, err := metav1.LabelSelectorAsSelector(&dep.Spec.ClusterSelector)
		if err != nil {
			return metav1.Condition{}, err
		}
		if !selector.Matches(labels.Set(cluster.Labels)) {
			return failed(addonsv1alpha1.ReasonDependencyNotApplicable,
				fmt.Sprintf("dependency %q exists but its selector never matches cluster %s", name, cluster.Name)), nil
		}

		release, err := r.findRelease(ctx, types.NamespacedName{Namespace: proxy.Namespace, Name: name}, cluster)
		if err != nil {
			return metav1.Condition{}, err
		}
		if release == nil ||
			(release.Status.Phase != addonsv1alpha1.PhaseDeployed && release.Status.Phase != addonsv1alpha1.PhaseCompleted) {
			return failed(addonsv1alpha1.ReasonWaitingForDependency,
				fmt.Sprintf("dependency %q is not yet deployed on cluster %s", name, cluster.Name)), nil
		}
	}
	return satisfied, nil
}

// findRelease returns the release realizing the given proxy on the given cluster, or nil. Found
// by its spec refs, not a name convention, matching how stampReleases adopts.
func (r *ZarfPackageProxyReconciler) findRelease(ctx context.Context, proxyKey types.NamespacedName, cluster *clusterv1.Cluster) (*addonsv1alpha1.ZarfPackageRelease, error) {
	releases := &addonsv1alpha1.ZarfPackageReleaseList{}
	if err := r.List(ctx, releases, client.InNamespace(cluster.Namespace)); err != nil {
		return nil, err
	}
	for i := range releases.Items {
		release := &releases.Items[i]
		if release.Spec.ClusterRef.Name != cluster.Name {
			continue
		}
		refNS := release.Spec.PackageProxyRef.Namespace
		if refNS == "" {
			refNS = release.Namespace
		}
		if release.Spec.PackageProxyRef.Name == proxyKey.Name && refNS == proxyKey.Namespace {
			return release, nil
		}
	}
	return nil, nil
}

// applyDependencyVerdicts sets the DependenciesSatisfied condition on each matched cluster's
// release. Clusters without a release (not initialized yet, name collision) are skipped; the
// verdict re-runs when the release appears.
func (r *ZarfPackageProxyReconciler) applyDependencyVerdicts(ctx context.Context, proxy *addonsv1alpha1.ZarfPackageProxy, matched []clusterv1.Cluster) error {
	for i := range matched {
		cluster := &matched[i]
		release, err := r.findRelease(ctx, client.ObjectKeyFromObject(proxy), cluster)
		if err != nil {
			return err
		}
		if release == nil {
			continue
		}
		condition, err := r.dependencyVerdict(ctx, proxy, cluster)
		if err != nil {
			return err
		}
		before := release.DeepCopy()
		if !meta.SetStatusCondition(&release.Status.Conditions, condition) {
			continue
		}
		if err := r.Status().Patch(ctx, release, client.MergeFrom(before)); err != nil {
			return err
		}
	}
	return nil
}
