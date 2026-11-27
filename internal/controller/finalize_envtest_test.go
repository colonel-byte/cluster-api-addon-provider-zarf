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
	"github.com/colonel-byte/cluster-api-addon-provider-zarf/internal/deploy"
	"github.com/colonel-byte/cluster-api-addon-provider-zarf/internal/envtest"
	"github.com/colonel-byte/cluster-api-addon-provider-zarf/internal/resolver"
)

// TestUninstallFinalizers drives §8B end to end against the envtest server playing both sides:
// the finalizer tracking the proxy's live policy, the removal Job on release deletion, retry
// after a failed removal, the nothing-deployed quiet drop, and §7C's teardown override.
func TestUninstallFinalizers(t *testing.T) {
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

	createProxy := func(t *testing.T, name string) (*ZarfPackageProxyReconciler, ctrl.Request) {
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
		if err := c.Create(ctx, proxy); err != nil {
			t.Fatalf("creating proxy %s: %v", name, err)
		}
		r := &ZarfPackageProxyReconciler{
			Client:      c,
			Resolver:    &stubResolver{result: resolver.Result{Digest: "sha256:aaaa", Name: name, APIVersion: "v1beta1"}, tagDigest: "sha256:aaaa"},
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
	releaseGone := func(t *testing.T, proxyName string) bool {
		t.Helper()
		err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: proxyName + "-edge-1"}, &addonsv1alpha1.ZarfPackageRelease{})
		if err != nil && !apierrors.IsNotFound(err) {
			t.Fatalf("reading release %s-edge-1: %v", proxyName, err)
		}
		return err != nil
	}
	readRemovalJob := func(t *testing.T, proxyName string) *batchv1.Job {
		t.Helper()
		job := &batchv1.Job{}
		err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: proxyName + "-edge-1-remove"}, job)
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			t.Fatalf("reading the removal Job: %v", err)
		}
		return job
	}
	deployRecord := func(t *testing.T, packageName string) *corev1.Secret {
		t.Helper()
		record := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: zarfconfig.ZarfPackagePrefix + packageName, Namespace: zarfstate.ZarfNamespaceName},
		}
		if err := c.Create(ctx, record); err != nil {
			t.Fatalf("creating the deployed-package record: %v", err)
		}
		return record
	}

	// unmatch rewrites the proxy's selector so its cluster no longer matches and reconciles:
	// stampReleases deletes the release, and §8B decides uninstall-vs-orphan from there.
	unmatch := func(t *testing.T, r *ZarfPackageProxyReconciler, req ctrl.Request) {
		t.Helper()
		proxy := &addonsv1alpha1.ZarfPackageProxy{}
		if err := c.Get(ctx, req.NamespacedName, proxy); err != nil {
			t.Fatalf("reading the proxy: %v", err)
		}
		before := proxy.DeepCopy()
		proxy.Spec.ClusterSelector = metav1.LabelSelector{MatchLabels: map[string]string{"tier": "none"}}
		if err := c.Patch(ctx, proxy, client.MergeFrom(before)); err != nil {
			t.Fatalf("patching the selector: %v", err)
		}
		// Twice: the first pass deletes the un-matched release, the second finalizes it.
		for range 2 {
			if _, err := r.Reconcile(ctx, req); err != nil {
				t.Fatalf("reconciling the un-match: %v", err)
			}
		}
	}

	// The finalizer tracks the proxy's live policy: born under the default Delete, stripped by
	// a flip to Retain, restored by the flip back.
	appR, appReq := createProxy(t, "app-a")
	release := readRelease(t, "app-a")
	if !slices.Contains(release.Finalizers, addonsv1alpha1.FinalizerRemoval) {
		t.Fatalf("a Delete-policy release carries no removal finalizer: %v", release.Finalizers)
	}
	setPolicy := func(t *testing.T, policy addonsv1alpha1.DeletionPolicy) {
		t.Helper()
		proxy := &addonsv1alpha1.ZarfPackageProxy{}
		if err := c.Get(ctx, appReq.NamespacedName, proxy); err != nil {
			t.Fatalf("reading the proxy: %v", err)
		}
		before := proxy.DeepCopy()
		proxy.Spec.DeletionPolicy = policy
		if err := c.Patch(ctx, proxy, client.MergeFrom(before)); err != nil {
			t.Fatalf("patching the policy: %v", err)
		}
		if _, err := appR.Reconcile(ctx, appReq); err != nil {
			t.Fatalf("reconciling after the policy flip: %v", err)
		}
	}
	setPolicy(t, addonsv1alpha1.PolicyRetain)
	if release = readRelease(t, "app-a"); slices.Contains(release.Finalizers, addonsv1alpha1.FinalizerRemoval) {
		t.Fatal("the finalizer survived a flip to Retain")
	}
	setPolicy(t, addonsv1alpha1.PolicyDelete)
	if release = readRelease(t, "app-a"); !slices.Contains(release.Finalizers, addonsv1alpha1.FinalizerRemoval) {
		t.Fatal("the finalizer did not return on the flip back to Delete")
	}

	// Un-matching the cluster while its package is deployed runs the removal Job, and only its
	// success lets the release go.
	record := deployRecord(t, "app-a")
	unmatch(t, appR, appReq)
	job := readRemovalJob(t, "app-a")
	if job == nil {
		t.Fatal("no removal Job was stamped")
	}
	wantArgs := []string{"remove", "--name", "app-a", "--cache-path", "/var/lib/caapz/cache"}
	if !slices.Equal(job.Spec.Template.Spec.Containers[0].Args, wantArgs) {
		t.Fatalf("removal Job args = %v, want %v", job.Spec.Template.Spec.Containers[0].Args, wantArgs)
	}
	release = readRelease(t, "app-a")
	if release.Status.Phase != addonsv1alpha1.PhaseRemoving {
		t.Fatalf("phase = %q, want Removing", release.Status.Phase)
	}
	finishJob(t, c, job, deploy.Result{Succeeded: true, Name: "app-a"}, batchv1.JobComplete)
	if _, err := appR.Reconcile(ctx, appReq); err != nil {
		t.Fatalf("reconciling after the removal completed: %v", err)
	}
	if !releaseGone(t, "app-a") {
		t.Fatal("the release survived its completed removal")
	}
	if err := c.Delete(ctx, record); err != nil {
		t.Fatalf("deleting the record: %v", err)
	}
	// app-a's released cluster slot frees only by Job deletion; drop the finished Job so the
	// later scenarios' slot checks see a quiet cluster.
	if err := c.Delete(ctx, job); err != nil {
		t.Fatalf("deleting the finished removal Job: %v", err)
	}

	// A failed removal surfaces RemovalFailed, deletes the failed Job, and retries on a timer.
	failR, failReq := createProxy(t, "app-b")
	record = deployRecord(t, "app-b")
	unmatch(t, failR, failReq)
	finishJob(t, c, readRemovalJob(t, "app-b"), deploy.Result{Error: "helm uninstall timed out"}, batchv1.JobFailed)
	result, err := failR.Reconcile(ctx, failReq)
	if err != nil {
		t.Fatalf("reconciling after the failed removal: %v", err)
	}
	if result.RequeueAfter != removalRetryDelay {
		t.Fatalf("RequeueAfter = %v, want %v", result.RequeueAfter, removalRetryDelay)
	}
	release = readRelease(t, "app-b")
	failed := meta.FindStatusCondition(release.Status.Conditions, addonsv1alpha1.ConditionRemovalFailed)
	if failed == nil || failed.Status != metav1.ConditionTrue || failed.Message != "helm uninstall timed out" {
		t.Fatalf("RemovalFailed = %+v, want True with the pod's error", failed)
	}
	if readRemovalJob(t, "app-b") != nil {
		t.Fatal("the failed removal Job was not deleted for the retry")
	}
	if _, err := failR.Reconcile(ctx, failReq); err != nil {
		t.Fatalf("reconciling the retry: %v", err)
	}
	job = readRemovalJob(t, "app-b")
	if job == nil {
		t.Fatal("the retry stamped no fresh removal Job")
	}
	finishJob(t, c, job, deploy.Result{Succeeded: true, Name: "app-b"}, batchv1.JobComplete)
	if _, err := failR.Reconcile(ctx, failReq); err != nil {
		t.Fatalf("reconciling after the retried removal: %v", err)
	}
	if !releaseGone(t, "app-b") {
		t.Fatal("the release survived its retried removal")
	}
	if err := c.Delete(ctx, record); err != nil {
		t.Fatalf("deleting the record: %v", err)
	}
	if err := c.Delete(ctx, job); err != nil {
		t.Fatalf("deleting the finished removal Job: %v", err)
	}

	// A release whose package never deployed has nothing to remove: no Job, quiet drop.
	noneR, noneReq := createProxy(t, "app-c")
	unmatch(t, noneR, noneReq)
	if !releaseGone(t, "app-c") {
		t.Fatal("a nothing-deployed release did not finalize quietly")
	}
	if readRemovalJob(t, "app-c") != nil {
		t.Fatal("a nothing-deployed release got a removal Job")
	}

	// §7C: the Cluster going away outranks uninstall - the finalizer drops without removal
	// even though the package's record says deployed. Last, because it deletes the Cluster.
	lastR, lastReq := createProxy(t, "app-d")
	record = deployRecord(t, "app-d")
	if err := c.Delete(ctx, cluster); err != nil {
		t.Fatalf("deleting the Cluster: %v", err)
	}
	if err := c.Delete(ctx, readRelease(t, "app-d")); err != nil {
		t.Fatalf("deleting the release: %v", err)
	}
	if _, err := lastR.Reconcile(ctx, lastReq); err != nil {
		t.Fatalf("reconciling under cluster teardown: %v", err)
	}
	if !releaseGone(t, "app-d") {
		t.Fatal("cluster teardown did not drop the finalizer")
	}
	if readRemovalJob(t, "app-d") != nil {
		t.Fatal("a removal Job was stamped toward a deleted Cluster")
	}
	if err := c.Delete(ctx, record); err != nil {
		t.Fatalf("deleting the record: %v", err)
	}
}
