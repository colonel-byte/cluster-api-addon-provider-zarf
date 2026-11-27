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
	"slices"
	"testing"

	zarfconfig "github.com/zarf-dev/zarf/src/config"
	zarfstate "github.com/zarf-dev/zarf/src/pkg/state"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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

// TestProxyFinalizer walks §8B's proxy deletion: the proxy's finalizer deletes its releases and
// waits for their own finalization - a removal Job under Delete, an immediate orphan under
// Retain - before the proxy goes away.
func TestProxyFinalizer(t *testing.T) {
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
	gone := func(t *testing.T, key types.NamespacedName, obj client.Object) bool {
		t.Helper()
		err := c.Get(ctx, key, obj)
		if err != nil && !apierrors.IsNotFound(err) {
			t.Fatalf("reading %s: %v", key, err)
		}
		return err != nil
	}

	// Delete policy: proxy deletion uninstalls through its release before either object goes.
	r, req := createProxy(t, "px-a")
	proxy := &addonsv1alpha1.ZarfPackageProxy{}
	if err := c.Get(ctx, req.NamespacedName, proxy); err != nil {
		t.Fatalf("reading the proxy: %v", err)
	}
	if !slices.Contains(proxy.Finalizers, addonsv1alpha1.FinalizerReleaseCleanup) {
		t.Fatalf("the proxy carries no release-cleanup finalizer: %v", proxy.Finalizers)
	}
	record := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: zarfconfig.ZarfPackagePrefix + "px-a", Namespace: zarfstate.ZarfNamespaceName},
	}
	if err := c.Create(ctx, record); err != nil {
		t.Fatalf("creating the deployed-package record: %v", err)
	}
	t.Cleanup(func() {
		_ = c.Delete(context.Background(), record) //nolint:errcheck // best-effort cleanup
	})
	if err := c.Delete(ctx, proxy); err != nil {
		t.Fatalf("deleting the proxy: %v", err)
	}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("reconciling the deleting proxy: %v", err)
	}
	releaseKey := types.NamespacedName{Namespace: ns, Name: "px-a-edge-1"}
	release := &addonsv1alpha1.ZarfPackageRelease{}
	if err := c.Get(ctx, releaseKey, release); err != nil {
		t.Fatalf("reading the release: %v", err)
	}
	if release.DeletionTimestamp.IsZero() {
		t.Fatal("proxy deletion did not delete the release")
	}
	job := &batchv1.Job{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: "px-a-edge-1-remove"}, job); err != nil {
		t.Fatalf("reading the removal Job: %v", err)
	}
	if gone(t, req.NamespacedName, &addonsv1alpha1.ZarfPackageProxy{}) {
		t.Fatal("the proxy went away before its release finalized")
	}
	finishJob(t, c, job, deploy.Result{Succeeded: true, Name: "px-a"}, batchv1.JobComplete)
	for range 2 {
		if _, err := r.Reconcile(ctx, req); err != nil {
			t.Fatalf("reconciling after the removal: %v", err)
		}
	}
	if !gone(t, releaseKey, &addonsv1alpha1.ZarfPackageRelease{}) {
		t.Fatal("the release survived the proxy's deletion")
	}
	if !gone(t, req.NamespacedName, &addonsv1alpha1.ZarfPackageProxy{}) {
		t.Fatal("the proxy survived its releases")
	}

	// Retain: the release goes immediately - no finalizer, no Job - and the proxy follows.
	r, req = createProxy(t, "px-b")
	proxy = &addonsv1alpha1.ZarfPackageProxy{}
	if err := c.Get(ctx, req.NamespacedName, proxy); err != nil {
		t.Fatalf("reading the proxy: %v", err)
	}
	before := proxy.DeepCopy()
	proxy.Spec.DeletionPolicy = addonsv1alpha1.PolicyRetain
	if err := c.Patch(ctx, proxy, client.MergeFrom(before)); err != nil {
		t.Fatalf("patching the policy: %v", err)
	}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("reconciling the Retain flip: %v", err)
	}
	if err := c.Delete(ctx, proxy); err != nil {
		t.Fatalf("deleting the proxy: %v", err)
	}
	for range 2 {
		if _, err := r.Reconcile(ctx, req); err != nil {
			t.Fatalf("reconciling the deleting proxy: %v", err)
		}
	}
	if !gone(t, types.NamespacedName{Namespace: ns, Name: "px-b-edge-1"}, &addonsv1alpha1.ZarfPackageRelease{}) {
		t.Fatal("a Retain release blocked proxy deletion")
	}
	if !gone(t, req.NamespacedName, &addonsv1alpha1.ZarfPackageProxy{}) {
		t.Fatal("the Retain proxy did not finalize")
	}
	if !gone(t, types.NamespacedName{Namespace: ns, Name: "px-b-edge-1-remove"}, &batchv1.Job{}) {
		t.Fatal("a Retain deletion stamped a removal Job")
	}
}
