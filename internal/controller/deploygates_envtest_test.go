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
	"context"
	"testing"

	zarfconfig "github.com/zarf-dev/zarf/src/config"
	zarfstate "github.com/zarf-dev/zarf/src/pkg/state"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	addonsv1alpha1 "github.com/colonel-byte/cluster-api-addon-provider-zarf/api/v1alpha1"
	"github.com/colonel-byte/cluster-api-addon-provider-zarf/internal/envtest"
	"github.com/colonel-byte/cluster-api-addon-provider-zarf/internal/resolver"
)

// TestDeployGates drives the §6 gating end to end against the envtest server playing both the
// management and the workload cluster: the init-first gate, the one-Job-per-cluster rule with
// its annotation opt-out, and the ApplyOnce probe-and-adopt.
func TestDeployGates(t *testing.T) {
	c := envtest.Client(t)
	ns := envtest.Namespace(t, c)
	ctx := t.Context()

	cluster := &clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: "edge-1", Namespace: ns, Labels: map[string]string{"tier": "edge"}},
		Spec:       clusterv1.ClusterSpec{Paused: ptr.To(false)},
	}
	if err := c.Create(ctx, cluster); err != nil {
		t.Fatalf("creating the Cluster: %v", err)
	}
	clusterBefore := cluster.DeepCopy()
	cluster.Status.Initialization = clusterv1.ClusterInitializationStatus{ControlPlaneInitialized: ptr.To(true)}
	if err := c.Status().Patch(ctx, cluster, client.MergeFrom(clusterBefore)); err != nil {
		t.Fatalf("marking the control plane initialized: %v", err)
	}
	kubeconfig := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "edge-1-kubeconfig", Namespace: ns},
		Data:       map[string][]byte{kubeconfigSecretKey: envtest.Kubeconfig(t)},
	}
	if err := c.Create(ctx, kubeconfig); err != nil {
		t.Fatalf("creating the kubeconfig secret: %v", err)
	}
	zarfNS := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: zarfstate.ZarfNamespaceName}}
	if err := c.Create(ctx, zarfNS); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("creating the zarf namespace: %v", err)
	}

	// createProxy stamps a proxy and reconciles it twice - resolution and release first, spec
	// and gating second - returning its reconciler for later passes.
	createProxy := func(t *testing.T, name string, result resolver.Result, mutate func(*addonsv1alpha1.ZarfPackageProxy)) (*ZarfPackageProxyReconciler, ctrl.Request) {
		t.Helper()
		proxy := &addonsv1alpha1.ZarfPackageProxy{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: addonsv1alpha1.ZarfPackageProxySpec{
				ClusterSelector: metav1.LabelSelector{MatchLabels: map[string]string{"tier": "edge"}},
				Package: &addonsv1alpha1.ZarfPackageSource{
					Repository: "registry.internal/zarf/" + name,
					Version:    "1.0.0",
				},
			},
		}
		if mutate != nil {
			mutate(proxy)
		}
		if err := c.Create(ctx, proxy); err != nil {
			t.Fatalf("creating proxy %s: %v", name, err)
		}
		r := &ZarfPackageProxyReconciler{
			Client:      c,
			Resolver:    &stubResolver{result: result, tagDigest: result.Digest},
			DeployImage: "registry.internal/caapz:test",
			CachePath:   "/var/lib/caapz/cache",
		}
		req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}}
		for range 2 {
			if _, err := r.Reconcile(ctx, req); err != nil {
				t.Fatalf("reconciling %s: %v", name, err)
			}
		}
		return r, req
	}

	readRelease := func(t *testing.T, proxyName string) *addonsv1alpha1.ZarfPackageRelease {
		t.Helper()
		release := &addonsv1alpha1.ZarfPackageRelease{}
		if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: proxyName + "-edge-1"}, release); err != nil {
			t.Fatalf("reading release %s-edge-1: %v", proxyName, err)
		}
		return release
	}
	jobExists := func(t *testing.T, proxyName string) bool {
		t.Helper()
		err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: proxyName + "-edge-1-deploy"}, &batchv1.Job{})
		if err != nil && !apierrors.IsNotFound(err) {
			t.Fatalf("reading the Job: %v", err)
		}
		return err == nil
	}

	// A plain package on a cluster with no zarf state holds behind the init-first gate.
	plainR, plainReq := createProxy(t, "plain-app", resolver.Result{
		Digest:     "sha256:aaaa",
		Name:       "plain-app",
		APIVersion: "v1beta1",
	}, nil)
	release := readRelease(t, "plain-app")
	if release.Status.Phase != addonsv1alpha1.PhasePending {
		t.Fatalf("plain-app phase = %q, want Pending", release.Status.Phase)
	}
	ready := meta.FindStatusCondition(release.Status.Conditions, addonsv1alpha1.ConditionReady)
	if ready == nil || ready.Reason != addonsv1alpha1.ReasonWaitingForZarfState {
		t.Fatalf("plain-app Ready = %+v, want reason WaitingForZarfState", ready)
	}
	if jobExists(t, "plain-app") {
		t.Fatal("plain-app got a Job despite the init gate")
	}

	// An init package is what the gate exists for: it takes the cluster's deploy slot.
	createProxy(t, "init-pkg", resolver.Result{
		Digest:        "sha256:bbbb",
		Name:          "init-pkg",
		IsInitPackage: true,
		APIVersion:    "v1beta1",
	}, nil)
	if !jobExists(t, "init-pkg") {
		t.Fatal("init-pkg got no Job; an init package must pass the init gate")
	}
	release = readRelease(t, "init-pkg")
	if release.Status.Phase != addonsv1alpha1.PhaseDeploying {
		t.Fatalf("init-pkg phase = %q, want Deploying", release.Status.Phase)
	}

	// With zarf state present the init gate opens, but init-pkg's running Job now holds the
	// one-Job-per-cluster slot.
	stateSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: zarfstate.ZarfStateSecretName, Namespace: zarfstate.ZarfNamespaceName},
	}
	if err := c.Create(ctx, stateSecret); err != nil {
		t.Fatalf("creating the zarf-state secret: %v", err)
	}
	t.Cleanup(func() {
		// t.Context is already canceled in cleanups; the shared secret must still go.
		_ = c.Delete(context.Background(), stateSecret) //nolint:errcheck // best-effort cleanup
	})
	if _, err := plainR.Reconcile(ctx, plainReq); err != nil {
		t.Fatalf("reconciling plain-app: %v", err)
	}
	release = readRelease(t, "plain-app")
	ready = meta.FindStatusCondition(release.Status.Conditions, addonsv1alpha1.ConditionReady)
	if ready == nil || ready.Reason != addonsv1alpha1.ReasonWaitingForActiveDeploy {
		t.Fatalf("plain-app Ready = %+v, want reason WaitingForActiveDeploy", ready)
	}
	if jobExists(t, "plain-app") {
		t.Fatal("plain-app got a Job while init-pkg's Job runs")
	}

	// The concurrent-deploys annotation opts the cluster out of the serialization.
	annotated := cluster.DeepCopy()
	cluster.Annotations = map[string]string{addonsv1alpha1.AnnotationConcurrentDeploys: "true"}
	if err := c.Patch(ctx, cluster, client.MergeFrom(annotated)); err != nil {
		t.Fatalf("annotating the cluster: %v", err)
	}
	if _, err := plainR.Reconcile(ctx, plainReq); err != nil {
		t.Fatalf("reconciling plain-app: %v", err)
	}
	if !jobExists(t, "plain-app") {
		t.Fatal("plain-app got no Job despite concurrent-deploys")
	}

	// An ApplyOnce proxy whose package already left a record on the cluster adopts it: phase
	// Completed, applied true, and no Job - even though no deploy ever ran here.
	record := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: zarfconfig.ZarfPackagePrefix + "seed-app", Namespace: zarfstate.ZarfNamespaceName},
	}
	if err := c.Create(ctx, record); err != nil {
		t.Fatalf("creating the deployed-package record: %v", err)
	}
	t.Cleanup(func() {
		_ = c.Delete(context.Background(), record) //nolint:errcheck // best-effort cleanup
	})
	seedR, seedReq := createProxy(t, "seed-app", resolver.Result{
		Digest:     "sha256:cccc",
		Name:       "seed-app",
		APIVersion: "v1beta1",
	}, func(p *addonsv1alpha1.ZarfPackageProxy) {
		p.Spec.Strategy = addonsv1alpha1.StrategyApplyOnce
	})
	release = readRelease(t, "seed-app")
	if release.Status.Phase != addonsv1alpha1.PhaseCompleted || !release.Status.Applied {
		t.Fatalf("seed-app phase = %q applied = %v, want Completed true", release.Status.Phase, release.Status.Applied)
	}
	ready = meta.FindStatusCondition(release.Status.Conditions, addonsv1alpha1.ConditionReady)
	if ready == nil || ready.Reason != addonsv1alpha1.ReasonAdoptedExistingRecord {
		t.Fatalf("seed-app Ready = %+v, want reason AdoptedExistingRecord", ready)
	}
	if jobExists(t, "seed-app") {
		t.Fatal("seed-app got a Job despite the existing record")
	}

	// Deleting the record alone does not re-seed: applied short-circuits without probing.
	if err := c.Delete(ctx, record); err != nil {
		t.Fatalf("deleting the record: %v", err)
	}
	if _, err := seedR.Reconcile(ctx, seedReq); err != nil {
		t.Fatalf("reconciling seed-app: %v", err)
	}
	release = readRelease(t, "seed-app")
	if release.Status.Phase != addonsv1alpha1.PhaseCompleted || jobExists(t, "seed-app") {
		t.Fatalf("seed-app re-ran after the record alone was deleted: phase %q", release.Status.Phase)
	}
}
