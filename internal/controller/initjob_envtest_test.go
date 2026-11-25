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
	"strings"
	"testing"

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
	"github.com/colonel-byte/cluster-api-addon-provider-zarf/internal/deployspec"
	"github.com/colonel-byte/cluster-api-addon-provider-zarf/internal/envtest"
	"github.com/colonel-byte/cluster-api-addon-provider-zarf/internal/resolver"
)

// TestAutoInitJob drives §6 step 5's auto-init deploy path end to end against the envtest
// server playing both clusters: the init Job stamped on a state-less cluster, the InitFailed
// absorb with edit-to-retry, and the reap-then-deploy handoff once the state exists.
func TestAutoInitJob(t *testing.T) {
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

	proxy := &addonsv1alpha1.ZarfPackageProxy{
		ObjectMeta: metav1.ObjectMeta{Name: "auto-app", Namespace: ns},
		Spec: addonsv1alpha1.ZarfPackageProxySpec{
			ClusterSelector: metav1.LabelSelector{MatchLabels: map[string]string{"tier": "edge"}},
			Package: addonsv1alpha1.ZarfPackageSource{
				Repository: "registry.internal/zarf/auto-app",
				Version:    "1.0.0",
			},
			Init: &addonsv1alpha1.InitOptions{
				AutoInit: true,
				Package: &addonsv1alpha1.ZarfPackageSource{
					Repository: "registry.internal/zarf/init",
					Version:    "v1.0.1",
					API:        addonsv1alpha1.PackageAPIV1Alpha1,
				},
				Set: map[string]string{"REGISTRY_STORAGE_CLASS": "fast"},
			},
		},
	}
	if err := c.Create(ctx, proxy); err != nil {
		t.Fatalf("creating the proxy: %v", err)
	}

	r := &ZarfPackageProxyReconciler{
		Client: c,
		Resolver: &routeResolver{
			results: map[string]resolver.Result{
				"registry.internal/zarf/auto-app": {
					Digest:     "sha256:aaaa",
					Name:       "auto-app",
					APIVersion: "v1beta1",
				},
				"registry.internal/zarf/init": {
					Digest:        "sha256:1217",
					Name:          "init",
					APIVersion:    "v1alpha1",
					IsInitPackage: true,
				},
			},
			calls: map[string]int{},
		},
		DeployImage: "registry.internal/caapz:test",
		CachePath:   "/var/lib/caapz/cache",
	}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "auto-app"}}
	reconcile := func(t *testing.T) {
		t.Helper()
		if _, err := r.Reconcile(ctx, req); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
	}
	readRelease := func(t *testing.T) *addonsv1alpha1.ZarfPackageRelease {
		t.Helper()
		release := &addonsv1alpha1.ZarfPackageRelease{}
		if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: "auto-app-edge-1"}, release); err != nil {
			t.Fatalf("reading the release: %v", err)
		}
		return release
	}
	getJob := func(t *testing.T, suffix string) *batchv1.Job {
		t.Helper()
		job := &batchv1.Job{}
		err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: "auto-app-edge-1" + suffix}, job)
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			t.Fatalf("reading the %s Job: %v", suffix, err)
		}
		return job
	}

	// Pass 1 stamps the release and pins both packages; pass 2 writes the spec and stamps the
	// init Job - never the deploy Job.
	reconcile(t)
	reconcile(t)
	initJob := getJob(t, "-init")
	if initJob == nil {
		t.Fatal("no init Job was stamped on the state-less cluster")
	}
	if getJob(t, "-deploy") != nil {
		t.Fatal("the main deploy Job was stamped before the cluster had zarf state")
	}
	args := strings.Join(initJob.Spec.Template.Spec.Containers[0].Args, " ")
	if !strings.Contains(args, deployspec.InitFileName) {
		t.Fatalf("the init Job does not read the init spec: %s", args)
	}
	if initJob.Annotations[specHashAnnotation] == "" {
		t.Fatal("the init Job carries no spec hash")
	}
	release := readRelease(t)
	if release.Status.Phase != addonsv1alpha1.PhaseDeploying {
		t.Fatalf("phase = %q, want Deploying", release.Status.Phase)
	}
	ready := meta.FindStatusCondition(release.Status.Conditions, addonsv1alpha1.ConditionReady)
	if ready == nil || ready.Reason != addonsv1alpha1.ReasonInitializing {
		t.Fatalf("Ready = %+v, want reason Initializing", ready)
	}

	// A failed init Job surfaces its pod's error as InitFailed and stays: edit-to-retry.
	finishJob(t, c, initJob, deploy.Result{Error: "injector refused to start"}, batchv1.JobFailed)
	reconcile(t)
	release = readRelease(t)
	if release.Status.Phase != addonsv1alpha1.PhaseFailed {
		t.Fatalf("phase = %q, want Failed", release.Status.Phase)
	}
	ready = meta.FindStatusCondition(release.Status.Conditions, addonsv1alpha1.ConditionReady)
	if ready == nil || ready.Reason != addonsv1alpha1.ReasonInitFailed ||
		!strings.Contains(ready.Message, "injector refused to start") {
		t.Fatalf("Ready = %+v, want InitFailed with the pod's error", ready)
	}
	if getJob(t, "-init") == nil {
		t.Fatal("the failed init Job was deleted with an unchanged spec")
	}

	// Editing the init options re-renders the init spec; the stale failed Job is deleted and a
	// fresh one stamped.
	beforeEdit := proxy.DeepCopy()
	proxy.Spec.Init.Set["REGISTRY_STORAGE_CLASS"] = "faster"
	if err := c.Patch(ctx, proxy, client.MergeFrom(beforeEdit)); err != nil {
		t.Fatalf("editing the init set: %v", err)
	}
	reconcile(t) // re-render the spec, absorb deletes the stale Job
	reconcile(t) // stamp the fresh init Job
	fresh := getJob(t, "-init")
	if fresh == nil {
		t.Fatal("no fresh init Job after the edit")
	}
	if fresh.UID == initJob.UID {
		t.Fatal("the stale init Job survived the edit")
	}

	// The init Job completes and the state it created exists: the Job is reaped and the main
	// deploy takes the freed slot.
	finishJob(t, c, fresh, deploy.Result{Succeeded: true, Name: "init"}, batchv1.JobComplete)
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
	reconcile(t)
	if getJob(t, "-init") != nil {
		t.Fatal("the completed init Job was not reaped once the state existed")
	}
	if getJob(t, "-deploy") == nil {
		t.Fatal("the main deploy Job was not stamped after init")
	}
	release = readRelease(t)
	if release.Status.Phase != addonsv1alpha1.PhaseDeploying {
		t.Fatalf("phase = %q, want Deploying on the main deploy", release.Status.Phase)
	}
}

// TestAutoInitFailureSurvivesState is the case the e2e run exposed: zarf writes its state
// secret early and can fail afterwards, so a cluster can carry state while the init that was
// meant to produce it failed. The release must hold with the failure visible rather than treat
// the state as proof of a finished init and deploy its package onto a half-initialized cluster.
func TestAutoInitFailureSurvivesState(t *testing.T) {
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
	before := cluster.DeepCopy()
	cluster.Status.Initialization = clusterv1.ClusterInitializationStatus{ControlPlaneInitialized: ptr.To(true)}
	if err := c.Status().Patch(ctx, cluster, client.MergeFrom(before)); err != nil {
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
	// The state secret is created later, part-way through: zarf writes it early in an init and
	// can then fail, which is the sequence this test exists for.
	stateSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: zarfstate.ZarfStateSecretName, Namespace: zarfstate.ZarfNamespaceName},
	}
	t.Cleanup(func() {
		_ = c.Delete(context.Background(), stateSecret) //nolint:errcheck // best-effort cleanup
	})

	proxy := &addonsv1alpha1.ZarfPackageProxy{
		ObjectMeta: metav1.ObjectMeta{Name: "half-init", Namespace: ns},
		Spec: addonsv1alpha1.ZarfPackageProxySpec{
			ClusterSelector: metav1.LabelSelector{MatchLabels: map[string]string{"tier": "edge"}},
			Package: addonsv1alpha1.ZarfPackageSource{
				Repository: "registry.internal/zarf/half-init",
				Version:    "1.0.0",
			},
			Init: &addonsv1alpha1.InitOptions{
				AutoInit: true,
				Package: &addonsv1alpha1.ZarfPackageSource{
					Repository: "registry.internal/zarf/init",
					Version:    "v1.0.1",
				},
			},
		},
	}
	if err := c.Create(ctx, proxy); err != nil {
		t.Fatalf("creating the proxy: %v", err)
	}

	r := &ZarfPackageProxyReconciler{
		Client: c,
		Resolver: &routeResolver{
			results: map[string]resolver.Result{
				"registry.internal/zarf/half-init": {
					Digest:     "sha256:aaaa",
					Name:       "half-init",
					APIVersion: "v1beta1",
				},
				"registry.internal/zarf/init": {
					Digest:        "sha256:1217",
					Name:          "init",
					APIVersion:    "v1beta1",
					IsInitPackage: true,
				},
			},
			calls: map[string]int{},
		},
		DeployImage: "registry.internal/caapz:test",
		CachePath:   "/var/lib/caapz/cache",
	}
	// On a state-less cluster the auto-init release takes the slot with an init Job.
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "half-init"}}
	for range 2 {
		if _, err := r.Reconcile(ctx, req); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
	}
	initJob := &batchv1.Job{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: "half-init-edge-1-init"}, initJob); err != nil {
		t.Fatalf("no init Job was stamped: %v", err)
	}

	// That Job fails - but zarf had already written state, so the next pass is the first to see
	// the failure and the state together.
	finishJob(t, c, initJob, deploy.Result{Error: "seed registry never came up"}, batchv1.JobFailed)
	if err := c.Create(ctx, stateSecret); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("creating the zarf-state secret: %v", err)
	}

	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("Reconcile after the failed init: %v", err)
	}
	release := &addonsv1alpha1.ZarfPackageRelease{}
	releaseKey := types.NamespacedName{Namespace: ns, Name: "half-init-edge-1"}
	if err := c.Get(ctx, releaseKey, release); err != nil {
		t.Fatalf("re-reading the release: %v", err)
	}
	ready := meta.FindStatusCondition(release.Status.Conditions, addonsv1alpha1.ConditionReady)
	if ready == nil || ready.Reason != addonsv1alpha1.ReasonInitFailed ||
		!strings.Contains(ready.Message, "seed registry never came up") {
		t.Fatalf("Ready = %+v, want InitFailed carrying the pod's error", ready)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(initJob), &batchv1.Job{}); err != nil {
		t.Fatalf("the failed init Job was reaped, losing the evidence: %v", err)
	}
	// The point of the whole fix: nothing deploys onto a cluster whose init failed.
	err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: "half-init-edge-1-deploy"}, &batchv1.Job{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("a deploy Job was stamped despite the failed init (err=%v)", err)
	}
}
