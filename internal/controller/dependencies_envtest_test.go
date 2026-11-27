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

// newTestProxy creates a proxy selecting tier=edge clusters.
// proxyRefs turns names into ZarfPackageProxy dependency references, which is what every
// dependency in this suite is - the kind field exists for bundles, which do not exist yet.
func proxyRefs(names ...string) []addonsv1alpha1.DependencyReference {
	refs := make([]addonsv1alpha1.DependencyReference, 0, len(names))
	for _, name := range names {
		refs = append(refs, addonsv1alpha1.DependencyReference{Name: name})
	}
	return refs
}

func newTestProxy(t *testing.T, c client.Client, ns, name string, dependsOn []string) {
	t.Helper()
	proxy := &addonsv1alpha1.ZarfPackageProxy{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: addonsv1alpha1.ZarfPackageProxySpec{
			ClusterSelector: metav1.LabelSelector{MatchLabels: map[string]string{"tier": "edge"}},
			Package: &addonsv1alpha1.ZarfPackageSource{
				Repository: "registry.internal/zarf/" + name,
				Version:    "1.0.0",
			},
			DependsOn: proxyRefs(dependsOn...),
		},
	}
	if err := c.Create(t.Context(), proxy); err != nil {
		t.Fatalf("unable to create proxy %s: %v", name, err)
	}
}

// TestDependencyVerdicts drives the §5B verdict table against a real API server: one initialized
// cluster, one dependent proxy, and a dependency walked through absent → non-matching → pending →
// deployed.
func TestDependencyVerdicts(t *testing.T) {
	c := envtest.Client(t)
	ns := envtest.Namespace(t, c)

	cluster := &clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: "edge-1", Namespace: ns, Labels: map[string]string{"tier": "edge"}},
		Spec:       clusterv1.ClusterSpec{Paused: ptr.To(false)},
	}
	if err := c.Create(t.Context(), cluster); err != nil {
		t.Fatalf("unable to create the Cluster: %v", err)
	}
	before := cluster.DeepCopy()
	cluster.Status.Initialization = clusterv1.ClusterInitializationStatus{ControlPlaneInitialized: ptr.To(true)}
	if err := c.Status().Patch(t.Context(), cluster, client.MergeFrom(before)); err != nil {
		t.Fatalf("unable to mark the control plane initialized: %v", err)
	}

	newTestProxy(t, c, ns, "logging", []string{"csi"})

	r := &ZarfPackageProxyReconciler{Client: c}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "logging"}}
	verdict := func(t *testing.T) *metav1.Condition {
		t.Helper()
		if _, err := r.Reconcile(t.Context(), req); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		release := &addonsv1alpha1.ZarfPackageRelease{}
		key := types.NamespacedName{Namespace: ns, Name: "logging-edge-1"}
		if err := c.Get(t.Context(), key, release); err != nil {
			t.Fatalf("unable to read the dependent release: %v", err)
		}
		condition := meta.FindStatusCondition(release.Status.Conditions, addonsv1alpha1.ConditionDependenciesSatisfied)
		if condition == nil {
			t.Fatal("no DependenciesSatisfied condition on the dependent release")
		}
		return condition
	}

	// The dependency proxy does not exist.
	if got := verdict(t); got.Status != metav1.ConditionFalse || got.Reason != addonsv1alpha1.ReasonDependencyNotFound {
		t.Fatalf("verdict = %s/%s, want False/%s", got.Status, got.Reason, addonsv1alpha1.ReasonDependencyNotFound)
	}

	// It exists but its selector never matches this cluster.
	csi := &addonsv1alpha1.ZarfPackageProxy{
		ObjectMeta: metav1.ObjectMeta{Name: "csi", Namespace: ns},
		Spec: addonsv1alpha1.ZarfPackageProxySpec{
			ClusterSelector: metav1.LabelSelector{MatchLabels: map[string]string{"tier": "core"}},
			Package: &addonsv1alpha1.ZarfPackageSource{
				Repository: "registry.internal/zarf/csi",
				Version:    "1.0.0",
			},
		},
	}
	if err := c.Create(t.Context(), csi); err != nil {
		t.Fatalf("unable to create the csi proxy: %v", err)
	}
	if got := verdict(t); got.Status != metav1.ConditionFalse || got.Reason != addonsv1alpha1.ReasonDependencyNotApplicable {
		t.Fatalf("verdict = %s/%s, want False/%s", got.Status, got.Reason, addonsv1alpha1.ReasonDependencyNotApplicable)
	}

	// It matches, but its release is pending.
	beforeSelector := csi.DeepCopy()
	csi.Spec.ClusterSelector = metav1.LabelSelector{MatchLabels: map[string]string{"tier": "edge"}}
	if err := c.Patch(t.Context(), csi, client.MergeFrom(beforeSelector)); err != nil {
		t.Fatalf("unable to relabel the csi selector: %v", err)
	}
	csiReq := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "csi"}}
	if _, err := r.Reconcile(t.Context(), csiReq); err != nil {
		t.Fatalf("Reconcile csi: %v", err)
	}
	if got := verdict(t); got.Status != metav1.ConditionFalse || got.Reason != addonsv1alpha1.ReasonWaitingForDependency {
		t.Fatalf("verdict = %s/%s, want False/%s", got.Status, got.Reason, addonsv1alpha1.ReasonWaitingForDependency)
	}

	// Its release deploys; the dependent is satisfied.
	csiRelease := &addonsv1alpha1.ZarfPackageRelease{}
	if err := c.Get(t.Context(), types.NamespacedName{Namespace: ns, Name: "csi-edge-1"}, csiRelease); err != nil {
		t.Fatalf("unable to read the csi release: %v", err)
	}
	beforePhase := csiRelease.DeepCopy()
	csiRelease.Status.Phase = addonsv1alpha1.PhaseDeployed
	if err := c.Status().Patch(t.Context(), csiRelease, client.MergeFrom(beforePhase)); err != nil {
		t.Fatalf("unable to mark the csi release deployed: %v", err)
	}
	if got := verdict(t); got.Status != metav1.ConditionTrue || got.Reason != addonsv1alpha1.ReasonDependenciesSatisfied {
		t.Fatalf("verdict = %s/%s, want True/%s", got.Status, got.Reason, addonsv1alpha1.ReasonDependenciesSatisfied)
	}
}

// TestDependencyCycle checks §5C: a cycle member reports DependenciesValid: False and stamps no
// release, and breaking the cycle clears both.
func TestDependencyCycle(t *testing.T) {
	c := envtest.Client(t)
	ns := envtest.Namespace(t, c)

	cluster := &clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: "edge-1", Namespace: ns, Labels: map[string]string{"tier": "edge"}},
		Spec:       clusterv1.ClusterSpec{Paused: ptr.To(false)},
	}
	if err := c.Create(t.Context(), cluster); err != nil {
		t.Fatalf("unable to create the Cluster: %v", err)
	}
	before := cluster.DeepCopy()
	cluster.Status.Initialization = clusterv1.ClusterInitializationStatus{ControlPlaneInitialized: ptr.To(true)}
	if err := c.Status().Patch(t.Context(), cluster, client.MergeFrom(before)); err != nil {
		t.Fatalf("unable to mark the control plane initialized: %v", err)
	}

	newTestProxy(t, c, ns, "a", []string{"b"})
	newTestProxy(t, c, ns, "b", []string{"a"})

	r := &ZarfPackageProxyReconciler{Client: c}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "a"}}
	if _, err := r.Reconcile(t.Context(), req); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	proxy := &addonsv1alpha1.ZarfPackageProxy{}
	if err := c.Get(t.Context(), req.NamespacedName, proxy); err != nil {
		t.Fatalf("unable to re-read the proxy: %v", err)
	}
	condition := meta.FindStatusCondition(proxy.Status.Conditions, addonsv1alpha1.ConditionDependenciesValid)
	if condition == nil || condition.Status != metav1.ConditionFalse || condition.Reason != addonsv1alpha1.ReasonDependencyCycle {
		t.Fatalf("DependenciesValid = %+v, want False/%s", condition, addonsv1alpha1.ReasonDependencyCycle)
	}
	releases := &addonsv1alpha1.ZarfPackageReleaseList{}
	if err := c.List(t.Context(), releases, client.InNamespace(ns)); err != nil {
		t.Fatalf("unable to list releases: %v", err)
	}
	if len(releases.Items) != 0 {
		t.Fatalf("a cycle member stamped %d releases, want 0", len(releases.Items))
	}

	// Break the cycle; the condition flips and the release stamps.
	b := &addonsv1alpha1.ZarfPackageProxy{}
	if err := c.Get(t.Context(), types.NamespacedName{Namespace: ns, Name: "b"}, b); err != nil {
		t.Fatalf("unable to read proxy b: %v", err)
	}
	beforeB := b.DeepCopy()
	b.Spec.DependsOn = nil
	if err := c.Patch(t.Context(), b, client.MergeFrom(beforeB)); err != nil {
		t.Fatalf("unable to break the cycle: %v", err)
	}
	if _, err := r.Reconcile(t.Context(), req); err != nil {
		t.Fatalf("Reconcile after break: %v", err)
	}
	if err := c.Get(t.Context(), req.NamespacedName, proxy); err != nil {
		t.Fatalf("unable to re-read the proxy: %v", err)
	}
	condition = meta.FindStatusCondition(proxy.Status.Conditions, addonsv1alpha1.ConditionDependenciesValid)
	if condition == nil || condition.Status != metav1.ConditionTrue {
		t.Fatalf("DependenciesValid after break = %+v, want True", condition)
	}
	if err := c.List(t.Context(), releases, client.InNamespace(ns)); err != nil {
		t.Fatalf("unable to list releases: %v", err)
	}
	if len(releases.Items) != 1 {
		t.Fatalf("got %d releases after the cycle broke, want 1", len(releases.Items))
	}
}
