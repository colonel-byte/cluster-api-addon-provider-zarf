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

//go:build envtest

package controller

import (
	"slices"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"

	addonsv1alpha1 "github.com/colonel-byte/cluster-api-addon-provider-zarf/api/v1alpha1"
	"github.com/colonel-byte/cluster-api-addon-provider-zarf/internal/envtest"
)

// TestClusterMatching drives Reconcile against a real API server and checks what lands in
// status.matchingClusters. Each case gets its own namespaces, which is what isolates it.
func TestClusterMatching(t *testing.T) {
	c := envtest.Client(t)

	newCluster := func(t *testing.T, ns, name string, labels map[string]string) {
		t.Helper()
		cluster := &clusterv1.Cluster{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: labels},
			Spec:       clusterv1.ClusterSpec{Paused: ptr.To(false)},
		}
		if err := c.Create(t.Context(), cluster); err != nil {
			t.Fatalf("unable to create Cluster %s/%s: %v", ns, name, err)
		}
	}
	tier := func(v string) map[string]string { return map[string]string{"tier": v} }

	tests := []struct {
		name string
		// selector applied to the proxy; nil means match-everything.
		selector map[string]string
		// clusterNamespaces is the --cluster-namespaces value, where "other" stands for the
		// second namespace created for the case.
		clusterNamespaces []string
		// clusters to create: namespace ("own"/"other"), name, labels.
		clusters []struct {
			ns     string
			name   string
			labels map[string]string
		}
		// want are the expected status entries, with "other/" standing for the second namespace.
		want []string
	}{
		{
			name:     "label match in own namespace",
			selector: tier("edge"),
			clusters: []struct {
				ns     string
				name   string
				labels map[string]string
			}{
				{
					ns:     "own",
					name:   "edge-1",
					labels: tier("edge"),
				},
				{
					ns:     "own",
					name:   "core-1",
					labels: tier("core"),
				},
			},
			want: []string{"edge-1"},
		},
		{
			name:     "empty selector matches every cluster in scope",
			selector: nil,
			clusters: []struct {
				ns     string
				name   string
				labels map[string]string
			}{
				{
					ns:     "own",
					name:   "a",
					labels: nil,
				},
				{
					ns:     "own",
					name:   "b",
					labels: tier("edge"),
				},
			},
			want: []string{"a", "b"},
		},
		{
			name:     "cross-namespace unreachable without the flag",
			selector: tier("edge"),
			clusters: []struct {
				ns     string
				name   string
				labels map[string]string
			}{
				{
					ns:     "other",
					name:   "edge-1",
					labels: tier("edge"),
				},
			},
			want: []string{},
		},
		{
			name:              "cross-namespace reachable when the flag names it",
			selector:          tier("edge"),
			clusterNamespaces: []string{"other"},
			clusters: []struct {
				ns     string
				name   string
				labels map[string]string
			}{
				{
					ns:     "own",
					name:   "edge-1",
					labels: tier("edge"),
				},
				{
					ns:     "other",
					name:   "edge-2",
					labels: tier("edge"),
				},
			},
			want: []string{"edge-1", "other/edge-2"},
		},
		{
			name:              "wildcard reaches any namespace",
			selector:          tier("edge"),
			clusterNamespaces: []string{"*"},
			clusters: []struct {
				ns     string
				name   string
				labels map[string]string
			}{
				{
					ns:     "other",
					name:   "edge-1",
					labels: tier("edge"),
				},
			},
			want: []string{"other/edge-1"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			own := envtest.Namespace(t, c)
			other := envtest.Namespace(t, c)
			resolve := func(ns string) string {
				if ns == "other" {
					return other
				}
				return own
			}

			// The wildcard case must only see its own namespaces; scope its selector with a
			// label unique to this test run, which the namespace generateName already is.
			selector := metav1.LabelSelector{MatchLabels: tt.selector}
			for _, cl := range tt.clusters {
				labels := map[string]string{"test-run": own}
				for k, v := range cl.labels {
					labels[k] = v
				}
				newCluster(t, resolve(cl.ns), cl.name, labels)
			}
			if tt.selector != nil {
				selector.MatchLabels = map[string]string{"test-run": own}
				for k, v := range tt.selector {
					selector.MatchLabels[k] = v
				}
			}

			proxy := &addonsv1alpha1.ZarfPackageProxy{
				ObjectMeta: metav1.ObjectMeta{Name: "match-test", Namespace: own},
				Spec: addonsv1alpha1.ZarfPackageProxySpec{
					ClusterSelector: selector,
					Package: &addonsv1alpha1.ZarfPackageSource{
						Repository: "registry.internal/zarf/podinfo",
						Version:    "1.0.0",
					},
				},
			}
			if err := c.Create(t.Context(), proxy); err != nil {
				t.Fatalf("unable to create the proxy: %v", err)
			}

			var clusterNamespaces []string
			for _, ns := range tt.clusterNamespaces {
				if ns == "*" {
					clusterNamespaces = append(clusterNamespaces, "*")
				} else {
					clusterNamespaces = append(clusterNamespaces, resolve(ns))
				}
			}
			r := &ZarfPackageProxyReconciler{
				Client:            c,
				ClusterNamespaces: clusterNamespaces,
			}
			req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: own, Name: proxy.Name}}
			if _, err := r.Reconcile(t.Context(), req); err != nil {
				t.Fatalf("Reconcile: %v", err)
			}

			if err := c.Get(t.Context(), req.NamespacedName, proxy); err != nil {
				t.Fatalf("unable to re-read the proxy: %v", err)
			}
			want := make([]string, 0, len(tt.want))
			for _, w := range tt.want {
				if rest, ok := strings.CutPrefix(w, "other/"); ok {
					w = other + "/" + rest
				}
				want = append(want, w)
			}
			got := proxy.Status.MatchingClusters
			if got == nil {
				got = []string{}
			}
			if !slices.Equal(got, want) {
				t.Errorf("status.matchingClusters = %v, want %v", got, want)
			}
			if proxy.Status.ObservedGeneration != proxy.Generation {
				t.Errorf("status.observedGeneration = %d, want %d", proxy.Status.ObservedGeneration, proxy.Generation)
			}
		})
	}
}
