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

	"slices"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	addonsv1alpha1 "github.com/colonel-byte/cluster-api-addon-provider-zarf/api/v1alpha1"
	"github.com/colonel-byte/cluster-api-addon-provider-zarf/internal/envtest"
)

// TestReleaseStamping walks one proxy through the stamping lifecycle against a real API server:
// no release before the control plane initializes, one controller-owned release after, deletion
// on selector un-match.
func TestReleaseStamping(t *testing.T) {
	c := envtest.Client(t)
	ns := envtest.Namespace(t, c)

	cluster := &clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: "edge-1", Namespace: ns, Labels: map[string]string{"tier": "edge"}},
		Spec:       clusterv1.ClusterSpec{Paused: ptr.To(false)},
	}
	if err := c.Create(t.Context(), cluster); err != nil {
		t.Fatalf("unable to create the Cluster: %v", err)
	}

	proxy := &addonsv1alpha1.ZarfPackageProxy{
		ObjectMeta: metav1.ObjectMeta{Name: "baseline", Namespace: ns},
		Spec: addonsv1alpha1.ZarfPackageProxySpec{
			ClusterSelector: metav1.LabelSelector{MatchLabels: map[string]string{"tier": "edge"}},
			Package: &addonsv1alpha1.ZarfPackageSource{
				Repository: "registry.internal/zarf/baseline",
				Version:    "1.0.0",
			},
		},
	}
	if err := c.Create(t.Context(), proxy); err != nil {
		t.Fatalf("unable to create the proxy: %v", err)
	}

	r := &ZarfPackageProxyReconciler{Client: c}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "baseline"}}
	reconcile := func(t *testing.T) {
		t.Helper()
		if _, err := r.Reconcile(t.Context(), req); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
	}
	releaseKey := types.NamespacedName{Namespace: ns, Name: "baseline-edge-1"}

	reconcile(t)
	release := &addonsv1alpha1.ZarfPackageRelease{}
	if err := c.Get(t.Context(), releaseKey, release); !apierrors.IsNotFound(err) {
		t.Fatalf("expected no release before the control plane initializes, got err=%v", err)
	}

	// Flip the v1beta2 initialization signal; the Cluster watch would re-enqueue in a running
	// manager, here the next Reconcile stands in for it.
	before := cluster.DeepCopy()
	cluster.Status.Initialization = clusterv1.ClusterInitializationStatus{ControlPlaneInitialized: ptr.To(true)}
	if err := c.Status().Patch(t.Context(), cluster, client.MergeFrom(before)); err != nil {
		t.Fatalf("unable to mark the control plane initialized: %v", err)
	}

	reconcile(t)
	if err := c.Get(t.Context(), releaseKey, release); err != nil {
		t.Fatalf("expected a stamped release: %v", err)
	}
	owner := metav1.GetControllerOf(release)
	if owner == nil || owner.Kind != "Cluster" || owner.Name != "edge-1" {
		t.Fatalf("release controller owner = %+v, want the Cluster", owner)
	}
	if release.Spec.ClusterRef.Name != "edge-1" || release.Spec.PackageProxyRef.Name != "baseline" {
		t.Fatalf("release spec = %+v, want refs to edge-1/baseline", release.Spec)
	}
	if release.Spec.PackageProxyRef.Namespace != "" {
		t.Fatalf("packageProxyRef.namespace = %q, want empty for a same-namespace proxy", release.Spec.PackageProxyRef.Namespace)
	}
	if release.Status.Phase != addonsv1alpha1.PhasePending {
		t.Fatalf("release phase = %q, want %q", release.Status.Phase, addonsv1alpha1.PhasePending)
	}

	// A second pass must adopt, not duplicate or error.
	reconcile(t)
	releases := &addonsv1alpha1.ZarfPackageReleaseList{}
	if err := c.List(t.Context(), releases, client.InNamespace(ns)); err != nil {
		t.Fatalf("unable to list releases: %v", err)
	}
	if len(releases.Items) != 1 {
		t.Fatalf("got %d releases after re-reconcile, want 1", len(releases.Items))
	}

	// Un-match the cluster; the release goes away.
	beforeLabels := cluster.DeepCopy()
	cluster.Labels = map[string]string{"tier": "core"}
	if err := c.Patch(t.Context(), cluster, client.MergeFrom(beforeLabels)); err != nil {
		t.Fatalf("unable to relabel the Cluster: %v", err)
	}
	reconcile(t)
	if err := c.Get(t.Context(), releaseKey, release); err != nil {
		t.Fatalf("reading the un-matched release: %v", err)
	}
	// Deleted, but §8B's removal finalizer holds it while finalizeReleases decides
	// uninstall-vs-orphan - the full walk is finalize_envtest_test.go's concern.
	if release.DeletionTimestamp.IsZero() {
		t.Fatal("expected the un-matched release to be deleting")
	}
	if !slices.Contains(release.Finalizers, addonsv1alpha1.FinalizerRemoval) {
		t.Fatalf("expected the removal finalizer on the deleting release, got %v", release.Finalizers)
	}
}
