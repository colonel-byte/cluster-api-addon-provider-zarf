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

	zarfstate "github.com/zarf-dev/zarf/src/pkg/state"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	addonsv1alpha1 "github.com/colonel-byte/cluster-api-addon-provider-zarf/api/v1alpha1"
	"github.com/colonel-byte/cluster-api-addon-provider-zarf/internal/deploy"
	"github.com/colonel-byte/cluster-api-addon-provider-zarf/internal/envtest"
	"github.com/colonel-byte/cluster-api-addon-provider-zarf/internal/resolver"
)

// finishJob plays the kubelet and Job controller: a pod with the given termination message,
// then the finishing condition on the Job.
func finishJob(t *testing.T, c client.Client, job *batchv1.Job, result deploy.Result, condition batchv1.JobConditionType) {
	t.Helper()
	ctx := t.Context()

	raw, err := yaml.Marshal(result)
	if err != nil {
		t.Fatalf("marshaling the result: %v", err)
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: job.Name + "-",
			Namespace:    job.Namespace,
			Labels: map[string]string{
				batchv1.JobNameLabel:       job.Name,
				batchv1.ControllerUidLabel: string(job.UID),
			},
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Containers:    []corev1.Container{{Name: "deploy", Image: "x"}},
		},
	}
	if err := c.Create(ctx, pod); err != nil {
		t.Fatalf("creating the pod: %v", err)
	}
	podBefore := pod.DeepCopy()
	pod.Status = corev1.PodStatus{
		Phase: corev1.PodSucceeded,
		ContainerStatuses: []corev1.ContainerStatus{{
			Name:  "deploy",
			Image: "x",
			State: corev1.ContainerState{
				Terminated: &corev1.ContainerStateTerminated{Message: string(raw)},
			},
		}},
	}
	if err := c.Status().Patch(ctx, pod, client.MergeFrom(podBefore)); err != nil {
		t.Fatalf("patching the pod status: %v", err)
	}

	// Finished-Job validation wants the whole story: start and completion times plus the
	// interim condition (SuccessCriteriaMet or FailureTarget) before the terminal one.
	jobBefore := job.DeepCopy()
	now := metav1.Now()
	job.Status.StartTime = &now
	interim := batchv1.JobSuccessCriteriaMet
	if condition == batchv1.JobFailed {
		interim = batchv1.JobFailureTarget
	} else {
		job.Status.CompletionTime = &now
	}
	job.Status.Conditions = append(job.Status.Conditions,
		batchv1.JobCondition{Type: interim, Status: corev1.ConditionTrue},
		batchv1.JobCondition{Type: condition, Status: corev1.ConditionTrue},
	)
	if err := c.Status().Patch(ctx, job, client.MergeFrom(jobBefore)); err != nil {
		t.Fatalf("patching the Job status: %v", err)
	}
}

// TestJobAbsorption finishes deploy Jobs by hand and checks the release reads them back:
// success into Deployed with the digest and version, an edit into a deleted stale Job and a
// fresh one, and failure into Failed with the pod's error.
func TestJobAbsorption(t *testing.T) {
	c := envtest.Client(t)
	ns := envtest.Namespace(t, c)
	ctx := t.Context()

	cluster := &clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: "edge-2", Namespace: ns, Labels: map[string]string{"tier": "edge"}},
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
		ObjectMeta: metav1.ObjectMeta{Name: "edge-2-kubeconfig", Namespace: ns},
		Data:       map[string][]byte{kubeconfigSecretKey: envtest.Kubeconfig(t)},
	}
	if err := c.Create(ctx, kubeconfig); err != nil {
		t.Fatalf("creating the kubeconfig secret: %v", err)
	}
	zarfNS := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: zarfstate.ZarfNamespaceName}}
	if err := c.Create(ctx, zarfNS); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("creating the zarf namespace: %v", err)
	}
	stateSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: zarfstate.ZarfStateSecretName, Namespace: zarfstate.ZarfNamespaceName},
	}
	if err := c.Create(ctx, stateSecret); err != nil {
		t.Fatalf("creating the zarf-state secret: %v", err)
	}
	t.Cleanup(func() {
		_ = c.Delete(context.Background(), stateSecret) //nolint:errcheck // best-effort cleanup
	})

	proxy := &addonsv1alpha1.ZarfPackageProxy{
		ObjectMeta: metav1.ObjectMeta{Name: "web-app", Namespace: ns},
		Spec: addonsv1alpha1.ZarfPackageProxySpec{
			ClusterSelector: metav1.LabelSelector{MatchLabels: map[string]string{"tier": "edge"}},
			Package: &addonsv1alpha1.ZarfPackageSource{
				Repository: "registry.internal/zarf/web-app",
				Version:    "2.0.0",
			},
		},
	}
	if err := c.Create(ctx, proxy); err != nil {
		t.Fatalf("creating the proxy: %v", err)
	}
	r := &ZarfPackageProxyReconciler{
		Client:      c,
		Resolver:    &stubResolver{result: resolver.Result{Digest: "sha256:aaaa", Name: "web-app", APIVersion: "v1beta1"}, tagDigest: "sha256:aaaa"},
		DeployImage: "registry.internal/caapz:test",
		CachePath:   "/var/lib/caapz/cache",
	}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "web-app"}}
	for range 2 {
		if _, err := r.Reconcile(ctx, req); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
	}

	jobKey := types.NamespacedName{Namespace: ns, Name: "web-app-edge-2-deploy"}
	job := &batchv1.Job{}
	if err := c.Get(ctx, jobKey, job); err != nil {
		t.Fatalf("the deploy Job was not stamped: %v", err)
	}
	firstHash := job.Annotations[specHashAnnotation]
	if firstHash == "" {
		t.Fatal("the Job carries no spec-hash annotation")
	}

	finishJob(t, c, job, deploy.Result{
		Succeeded: true,
		Package:   "registry.internal/zarf/web-app@sha256:aaaa",
		Name:      "web-app",
		Version:   "2.0.0",
	}, batchv1.JobComplete)
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("Reconcile after completion: %v", err)
	}

	release := &addonsv1alpha1.ZarfPackageRelease{}
	releaseKey := types.NamespacedName{Namespace: ns, Name: "web-app-edge-2"}
	if err := c.Get(ctx, releaseKey, release); err != nil {
		t.Fatalf("reading the release: %v", err)
	}
	if release.Status.Phase != addonsv1alpha1.PhaseDeployed {
		t.Fatalf("phase = %q, want Deployed", release.Status.Phase)
	}
	if release.Status.DeployedDigest != "sha256:aaaa" || release.Status.DeployedVersion != "2.0.0" {
		t.Fatalf("deployedDigest = %q deployedVersion = %q", release.Status.DeployedDigest, release.Status.DeployedVersion)
	}
	ready := meta.FindStatusCondition(release.Status.Conditions, addonsv1alpha1.ConditionReady)
	if ready == nil || ready.Status != metav1.ConditionTrue || ready.Reason != addonsv1alpha1.ReasonDeployed {
		t.Fatalf("Ready = %+v, want True/Deployed", ready)
	}

	// An edit re-renders the spec; the finished Job is stale, so it is deleted and the next
	// pass stamps a fresh one under the new hash.
	proxyBefore := proxy.DeepCopy()
	proxy.Spec.Values = &apiextensionsv1.JSON{Raw: []byte(`{"replicas":"3"}`)}
	if err := c.Patch(ctx, proxy, client.MergeFrom(proxyBefore)); err != nil {
		t.Fatalf("editing the proxy: %v", err)
	}
	for range 2 {
		if _, err := r.Reconcile(ctx, req); err != nil {
			t.Fatalf("Reconcile after the edit: %v", err)
		}
	}
	job = &batchv1.Job{}
	if err := c.Get(ctx, jobKey, job); err != nil {
		t.Fatalf("no replacement Job after the edit: %v", err)
	}
	if job.Annotations[specHashAnnotation] == firstHash {
		t.Fatal("the replacement Job carries the stale spec hash")
	}
	if len(job.Status.Conditions) != 0 {
		t.Fatal("the stale Job was not replaced")
	}

	// A failed Job reads back as Failed with the pod's error.
	finishJob(t, c, job, deploy.Result{
		Package: "registry.internal/zarf/web-app@sha256:aaaa",
		Error:   "helm install failed: context deadline exceeded",
	}, batchv1.JobFailed)
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("Reconcile after failure: %v", err)
	}
	if err := c.Get(ctx, releaseKey, release); err != nil {
		t.Fatalf("re-reading the release: %v", err)
	}
	if release.Status.Phase != addonsv1alpha1.PhaseFailed {
		t.Fatalf("phase = %q, want Failed", release.Status.Phase)
	}
	ready = meta.FindStatusCondition(release.Status.Conditions, addonsv1alpha1.ConditionReady)
	if ready == nil || ready.Reason != addonsv1alpha1.ReasonDeployFailed ||
		ready.Message != "helm install failed: context deadline exceeded" {
		t.Fatalf("Ready = %+v, want DeployFailed with the pod's error", ready)
	}
}

// TestStagedOrderingWalk drives a two-stage proxy through the walk against a real API server:
// stages run one at a time in declared order, each completion writes the applied record, a
// converged release reports Deployed, and editing one stage's values re-runs that stage alone.
func TestStagedOrderingWalk(t *testing.T) {
	c := envtest.Client(t)
	ns := envtest.Namespace(t, c)
	ctx := t.Context()

	cluster := &clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: "staged-2", Namespace: ns, Labels: map[string]string{"tier": "walk"}},
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
		ObjectMeta: metav1.ObjectMeta{Name: "staged-2-kubeconfig", Namespace: ns},
		Data:       map[string][]byte{kubeconfigSecretKey: envtest.Kubeconfig(t)},
	}
	if err := c.Create(ctx, kubeconfig); err != nil {
		t.Fatalf("creating the kubeconfig secret: %v", err)
	}
	zarfNS := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: zarfstate.ZarfNamespaceName}}
	if err := c.Create(ctx, zarfNS); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("creating the zarf namespace: %v", err)
	}
	stateSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: zarfstate.ZarfStateSecretName, Namespace: zarfstate.ZarfNamespaceName},
	}
	if err := c.Create(ctx, stateSecret); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("creating the zarf-state secret: %v", err)
	}
	t.Cleanup(func() {
		_ = c.Delete(context.Background(), stateSecret) //nolint:errcheck // best-effort cleanup
	})

	proxy := &addonsv1alpha1.ZarfPackageProxy{
		ObjectMeta: metav1.ObjectMeta{Name: "walk", Namespace: ns},
		Spec: addonsv1alpha1.ZarfPackageProxySpec{
			ClusterSelector: metav1.LabelSelector{MatchLabels: map[string]string{"tier": "walk"}},
			Packages: map[string]addonsv1alpha1.ZarfPackageSource{
				"a": {Repository: "registry.internal/zarf/a", Version: "1.0.0"},
				"b": {Repository: "registry.internal/zarf/b", Version: "2.0.0"},
			},
			Stages: []addonsv1alpha1.PackageStage{
				{Name: "alpha", PackageRef: "a"},
				{Name: "beta", PackageRef: "b"},
			},
		},
	}
	if err := c.Create(ctx, proxy); err != nil {
		t.Fatalf("creating the staged proxy: %v", err)
	}
	r := &ZarfPackageProxyReconciler{
		Client:      c,
		Resolver:    &stubResolver{result: resolver.Result{Digest: "sha256:aaaa", Name: "pkg", APIVersion: "v1beta1"}, tagDigest: "sha256:aaaa"},
		DeployImage: "registry.internal/caapz:test",
		CachePath:   "/var/lib/caapz/cache",
	}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "walk"}}
	reconcile := func(times int) {
		t.Helper()
		for range times {
			if _, err := r.Reconcile(ctx, req); err != nil {
				t.Fatalf("Reconcile: %v", err)
			}
		}
	}
	reconcile(3)

	alphaKey := types.NamespacedName{Namespace: ns, Name: "walk-staged-2-alpha-deploy"}
	betaKey := types.NamespacedName{Namespace: ns, Name: "walk-staged-2-beta-deploy"}
	alphaJob := &batchv1.Job{}
	if err := c.Get(ctx, alphaKey, alphaJob); err != nil {
		t.Fatalf("stage alpha's Job was not stamped: %v", err)
	}
	if alphaJob.Labels[stageLabel] != "alpha" {
		t.Errorf("stage label = %q", alphaJob.Labels[stageLabel])
	}
	if err := c.Get(ctx, betaKey, &batchv1.Job{}); err == nil {
		t.Fatal("stage beta's Job exists before alpha settled - the walk is not ordered")
	}

	finishJob(t, c, alphaJob, deploy.Result{
		Succeeded: true,
		Package:   "registry.internal/zarf/a@sha256:aaaa",
		Name:      "a",
		Version:   "1.0.0",
	}, batchv1.JobComplete)
	reconcile(2)

	betaJob := &batchv1.Job{}
	if err := c.Get(ctx, betaKey, betaJob); err != nil {
		t.Fatalf("stage beta's Job was not stamped after alpha settled: %v", err)
	}
	firstBetaUID := betaJob.UID
	finishJob(t, c, betaJob, deploy.Result{
		Succeeded: true,
		Package:   "registry.internal/zarf/b@sha256:aaaa",
		Name:      "b",
		Version:   "2.0.0",
	}, batchv1.JobComplete)
	reconcile(2)

	release := &addonsv1alpha1.ZarfPackageRelease{}
	releaseKey := types.NamespacedName{Namespace: ns, Name: "walk-staged-2"}
	if err := c.Get(ctx, releaseKey, release); err != nil {
		t.Fatalf("reading the release: %v", err)
	}
	if release.Status.Phase != addonsv1alpha1.PhaseDeployed {
		t.Fatalf("phase = %q, want Deployed; conditions: %+v", release.Status.Phase, release.Status.Conditions)
	}
	if len(release.Status.Stages) != 2 || release.Status.Stages[0].Name != "alpha" || release.Status.Stages[1].Name != "beta" {
		t.Fatalf("applied records = %+v", release.Status.Stages)
	}
	if release.Status.Stages[0].SpecHash == "" || release.Status.Stages[0].DeployedVersion != "1.0.0" {
		t.Fatalf("alpha's record is incomplete: %+v", release.Status.Stages[0])
	}
	if release.Status.StageCount != 2 || release.Status.AppliedStageCount != 2 {
		t.Fatalf("stage counts = %d/%d, want 2/2", release.Status.AppliedStageCount, release.Status.StageCount)
	}
	proxyNow := &addonsv1alpha1.ZarfPackageProxy{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: "walk"}, proxyNow); err != nil {
		t.Fatalf("re-reading the proxy: %v", err)
	}
	if proxyNow.Status.StageCount != 2 || proxyNow.Status.Version != "2.0.0" {
		t.Fatalf("proxy stageCount/version = %d/%q, want 2/2.0.0", proxyNow.Status.StageCount, proxyNow.Status.Version)
	}
	alphaHash := release.Status.Stages[0].SpecHash

	// Edit stage beta's values: beta alone re-runs.
	fresh := &addonsv1alpha1.ZarfPackageProxy{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: "walk"}, fresh); err != nil {
		t.Fatalf("re-reading the proxy: %v", err)
	}
	fresh.Spec.Stages[1].Values = &apiextensionsv1.JSON{Raw: []byte(`{"marker":"edited"}`)}
	if err := c.Update(ctx, fresh); err != nil {
		t.Fatalf("editing stage beta: %v", err)
	}
	reconcile(3)

	rerun := &batchv1.Job{}
	if err := c.Get(ctx, betaKey, rerun); err != nil {
		t.Fatalf("stage beta's Job was not re-stamped after the edit: %v", err)
	}
	if rerun.UID == firstBetaUID {
		t.Fatal("stage beta's Job was not replaced after the edit")
	}
	stillAlpha := &batchv1.Job{}
	if err := c.Get(ctx, alphaKey, stillAlpha); err != nil {
		t.Fatalf("stage alpha's Job disappeared on another stage's edit: %v", err)
	}
	if stillAlpha.UID != alphaJob.UID {
		t.Fatal("stage alpha re-ran on another stage's edit")
	}
	if err := c.Get(ctx, releaseKey, release); err != nil {
		t.Fatalf("re-reading the release: %v", err)
	}
	if got := findStageState(release, "alpha"); got == nil || got.SpecHash != alphaHash {
		t.Fatalf("alpha's applied record changed on another stage's edit: %+v", got)
	}
}
