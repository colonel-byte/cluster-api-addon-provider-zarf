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
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	addonsv1alpha1 "github.com/colonel-byte/cluster-api-addon-provider-zarf/api/v1alpha1"
	"github.com/colonel-byte/cluster-api-addon-provider-zarf/internal/envtest"
)

// TestReadyAggregate walks the fleet roll-up against a real API server: Unknown while the
// selector matches nothing, False with counts once clusters match but nothing has deployed,
// and True once every release reports Deployed. The counts are the part a condition alone
// cannot carry, and the part the printer columns read.
func TestReadyAggregate(t *testing.T) {
	c := envtest.Client(t)
	ns := envtest.Namespace(t, c)

	proxy := &addonsv1alpha1.ZarfPackageProxy{
		ObjectMeta: metav1.ObjectMeta{Name: "rollup", Namespace: ns},
		Spec: addonsv1alpha1.ZarfPackageProxySpec{
			ClusterSelector: metav1.LabelSelector{MatchLabels: map[string]string{"tier": "edge"}},
			Package: &addonsv1alpha1.ZarfPackageSource{
				Repository: "registry.internal/zarf/baseline",
				Version:    "1.0.0",
			},
		},
	}
	if err := c.Create(t.Context(), proxy); err != nil {
		t.Fatalf("creating the proxy: %v", err)
	}

	r := &ZarfPackageProxyReconciler{Client: c}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "rollup"}}
	reconcile := func(t *testing.T) *addonsv1alpha1.ZarfPackageProxy {
		t.Helper()
		if _, err := r.Reconcile(t.Context(), req); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		current := &addonsv1alpha1.ZarfPackageProxy{}
		if err := c.Get(t.Context(), req.NamespacedName, current); err != nil {
			t.Fatalf("re-reading the proxy: %v", err)
		}
		return current
	}
	readyOf := func(t *testing.T, p *addonsv1alpha1.ZarfPackageProxy) *metav1.Condition {
		t.Helper()
		condition := meta.FindStatusCondition(p.Status.Conditions, addonsv1alpha1.ConditionReady)
		if condition == nil {
			t.Fatal("the proxy carries no Ready condition")
		}
		return condition
	}

	// No cluster matches yet.
	current := reconcile(t)
	if condition := readyOf(t, current); condition.Status != metav1.ConditionUnknown ||
		condition.Reason != addonsv1alpha1.ReasonNoMatchingClusters {
		t.Fatalf("want Unknown/%s, got %s/%s", addonsv1alpha1.ReasonNoMatchingClusters, condition.Status, condition.Reason)
	}
	if current.Status.ClusterCount != 0 || current.Status.ReadyClusterCount != 0 {
		t.Fatalf("want 0/0 clusters, got %d/%d", current.Status.ReadyClusterCount, current.Status.ClusterCount)
	}

	// Two matching clusters with initialized control planes, so releases get stamped.
	for _, name := range []string{"edge-1", "edge-2"} {
		cluster := &clusterv1.Cluster{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: map[string]string{"tier": "edge"}},
			Spec:       clusterv1.ClusterSpec{Paused: ptr.To(false)},
		}
		if err := c.Create(t.Context(), cluster); err != nil {
			t.Fatalf("creating Cluster %s: %v", name, err)
		}
		before := cluster.DeepCopy()
		cluster.Status.Initialization = clusterv1.ClusterInitializationStatus{ControlPlaneInitialized: ptr.To(true)}
		if err := c.Status().Patch(t.Context(), cluster, client.MergeFrom(before)); err != nil {
			t.Fatalf("initializing Cluster %s: %v", name, err)
		}
	}

	current = reconcile(t)
	if condition := readyOf(t, current); condition.Status != metav1.ConditionFalse {
		t.Fatalf("want False while nothing has deployed, got %s/%s", condition.Status, condition.Reason)
	}
	if current.Status.ClusterCount != 2 || current.Status.ReadyClusterCount != 0 {
		t.Fatalf("want 0/2 clusters deployed, got %d/%d", current.Status.ReadyClusterCount, current.Status.ClusterCount)
	}

	// Deploy one of the two: the condition stays False, and the counts are what distinguish
	// this from a fleet that deployed nowhere.
	markDeployed(t, c, ns, "rollup-edge-1")
	current = reconcile(t)
	if condition := readyOf(t, current); condition.Status != metav1.ConditionFalse {
		t.Fatalf("want False with one of two deployed, got %s", condition.Status)
	}
	if current.Status.ReadyClusterCount != 1 || current.Status.ClusterCount != 2 {
		t.Fatalf("want 1/2 clusters deployed, got %d/%d", current.Status.ReadyClusterCount, current.Status.ClusterCount)
	}

	// Deploy the second, and the fleet has converged.
	markDeployed(t, c, ns, "rollup-edge-2")
	current = reconcile(t)
	condition := readyOf(t, current)
	if condition.Status != metav1.ConditionTrue || condition.Reason != addonsv1alpha1.ReasonConverged {
		t.Fatalf("want True/%s, got %s/%s: %s", addonsv1alpha1.ReasonConverged, condition.Status, condition.Reason, condition.Message)
	}
	if current.Status.ReadyClusterCount != 2 || current.Status.ClusterCount != 2 {
		t.Fatalf("want 2/2 clusters deployed, got %d/%d", current.Status.ReadyClusterCount, current.Status.ClusterCount)
	}
}

// markDeployed puts a stamped release into Deployed, which is what the roll-up counts.
func markDeployed(t *testing.T, c client.Client, ns, name string) {
	t.Helper()
	release := &addonsv1alpha1.ZarfPackageRelease{}
	if err := c.Get(t.Context(), types.NamespacedName{Namespace: ns, Name: name}, release); err != nil {
		t.Fatalf("reading release %s: %v", name, err)
	}
	before := release.DeepCopy()
	release.Status.Phase = addonsv1alpha1.PhaseDeployed
	if err := c.Status().Patch(t.Context(), release, client.MergeFrom(before)); err != nil {
		t.Fatalf("marking release %s deployed: %v", name, err)
	}
}
