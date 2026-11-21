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
	"slices"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	addonsv1alpha1 "github.com/colonel-byte/cluster-api-addon-provider-zarf/api/v1alpha1"
)

// matchClusters lists the Clusters the proxy's selector currently matches, in name order.
//
// The selection scope is the proxy's own namespace plus whatever --cluster-namespaces permits
// (docs/dev/architecture.md §2C): the flag widens where the selector reaches, so a namespace the
// flag does not name simply is not searched - there is no per-proxy way to ask for more, which is
// why no CrossNamespaceNotEnabled condition arises here. An empty selector matches every Cluster
// in scope, the usual Kubernetes convention.
func (r *ZarfPackageProxyReconciler) matchClusters(ctx context.Context, proxy *addonsv1alpha1.ZarfPackageProxy) ([]clusterv1.Cluster, error) {
	selector, err := metav1.LabelSelectorAsSelector(&proxy.Spec.ClusterSelector)
	if err != nil {
		return nil, err
	}

	namespaces := []string{proxy.Namespace}
	for _, ns := range r.ClusterNamespaces {
		if ns == "*" {
			namespaces = nil // every namespace, one unscoped list
			break
		}
		if ns != proxy.Namespace {
			namespaces = append(namespaces, ns)
		}
	}

	var matched []clusterv1.Cluster
	list := func(opts ...client.ListOption) error {
		clusters := &clusterv1.ClusterList{}
		if err := r.List(ctx, clusters, append(opts, client.MatchingLabelsSelector{Selector: selector})...); err != nil {
			return err
		}
		matched = append(matched, clusters.Items...)
		return nil
	}
	if namespaces == nil {
		if err := list(); err != nil {
			return nil, err
		}
	} else {
		for _, ns := range namespaces {
			if err := list(client.InNamespace(ns)); err != nil {
				return nil, err
			}
		}
	}

	// The proxy's own namespace sorts first - its entries read bare in status.matchingClusters,
	// so they lead - then the rest by namespace.
	slices.SortFunc(matched, func(a, b clusterv1.Cluster) int {
		if a.Namespace != b.Namespace {
			switch proxy.Namespace {
			case a.Namespace:
				return -1
			case b.Namespace:
				return 1
			}
			return strings.Compare(a.Namespace, b.Namespace)
		}
		return strings.Compare(a.Name, b.Name)
	})
	return matched, nil
}

// clusterToProxies maps a Cluster event to every proxy whose selector could reach it: proxies in
// the Cluster's namespace always, and proxies elsewhere only when --cluster-namespaces lets them
// select into it. Routing only - the actual match re-runs in Reconcile.
func (r *ZarfPackageProxyReconciler) clusterToProxies(ctx context.Context, obj client.Object) []reconcile.Request {
	crossNamespace := false
	for _, ns := range r.ClusterNamespaces {
		if ns == "*" || ns == obj.GetNamespace() {
			crossNamespace = true
			break
		}
	}

	proxies := &addonsv1alpha1.ZarfPackageProxyList{}
	opts := []client.ListOption{}
	if !crossNamespace {
		opts = append(opts, client.InNamespace(obj.GetNamespace()))
	}
	if err := r.List(ctx, proxies, opts...); err != nil {
		return nil
	}

	requests := make([]reconcile.Request, 0, len(proxies.Items))
	for _, proxy := range proxies.Items {
		requests = append(requests, reconcile.Request{NamespacedName: types.NamespacedName{
			Namespace: proxy.Namespace,
			Name:      proxy.Name,
		}})
	}
	return requests
}
