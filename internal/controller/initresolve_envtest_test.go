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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	addonsv1alpha1 "github.com/colonel-byte/cluster-api-addon-provider-zarf/api/v1alpha1"
	"github.com/colonel-byte/cluster-api-addon-provider-zarf/internal/deployspec"
	"github.com/colonel-byte/cluster-api-addon-provider-zarf/internal/envtest"
	"github.com/colonel-byte/cluster-api-addon-provider-zarf/internal/resolver"
)

// routeResolver routes by repository, so one test can resolve the main package and the init
// package to different results.
type routeResolver struct {
	results map[string]resolver.Result
	calls   map[string]int
}

func (s *routeResolver) Resolve(_ context.Context, repository, _ string, _ resolver.Options) (resolver.Result, error) {
	s.calls[repository]++
	return s.results[repository], nil
}

func (s *routeResolver) ResolveDigest(_ context.Context, repository, _ string, _ resolver.Options) (string, error) {
	return s.results[repository].Digest, nil
}

// TestInitPackageResolution walks §6 step 5's init resolution lifecycle: pin with the
// InitPackageResolved condition, the init spec rendered beside the main one, pin-once on
// unrelated edits, the NotAnInitPackage refusal, and a cleared status once auto-init is
// un-declared.
func TestInitPackageResolution(t *testing.T) {
	c := envtest.Client(t)
	ns := envtest.Namespace(t, c)

	cluster := &clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: "edge-1", Namespace: ns, Labels: map[string]string{"tier": "edge"}},
		Spec:       clusterv1.ClusterSpec{Paused: ptr.To(false)},
	}
	if err := c.Create(t.Context(), cluster); err != nil {
		t.Fatalf("creating the Cluster: %v", err)
	}
	before := cluster.DeepCopy()
	cluster.Status.Initialization = clusterv1.ClusterInitializationStatus{ControlPlaneInitialized: ptr.To(true)}
	if err := c.Status().Patch(t.Context(), cluster, client.MergeFrom(before)); err != nil {
		t.Fatalf("marking the control plane initialized: %v", err)
	}

	proxy := &addonsv1alpha1.ZarfPackageProxy{
		ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: ns},
		Spec: addonsv1alpha1.ZarfPackageProxySpec{
			ClusterSelector: metav1.LabelSelector{MatchLabels: map[string]string{"tier": "edge"}},
			Package: addonsv1alpha1.ZarfPackageSource{
				Repository: "registry.internal/zarf/app",
				Version:    "1.0.0",
			},
			Init: &addonsv1alpha1.InitOptions{
				AutoInit: true,
				Package: &addonsv1alpha1.ZarfPackageSource{
					Repository: "registry.internal/zarf/init",
					Version:    "v1.0.1",
					API:        addonsv1alpha1.PackageAPIV1Alpha1,
				},
				Set: map[string]string{
					"REGISTRY_STORAGE_CLASS": "{{ .Cluster.metadata.name }}-sc",
				},
			},
		},
	}
	if err := c.Create(t.Context(), proxy); err != nil {
		t.Fatalf("creating the proxy: %v", err)
	}

	stub := &routeResolver{
		results: map[string]resolver.Result{
			"registry.internal/zarf/app": {
				Digest:     "sha256:feed",
				Name:       "app",
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
	}
	r := &ZarfPackageProxyReconciler{Client: c, Resolver: stub}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "app"}}
	reconcile := func(t *testing.T) {
		t.Helper()
		if _, err := r.Reconcile(t.Context(), req); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		if err := c.Get(t.Context(), req.NamespacedName, proxy); err != nil {
			t.Fatalf("re-reading the proxy: %v", err)
		}
	}

	// The first pass stamps the release and pins both digests; the second writes the spec.
	reconcile(t)
	reconcile(t)

	if proxy.Status.InitResolvedDigest != "sha256:1217" ||
		!strings.HasSuffix(proxy.Status.InitResolvedRef, ":v1.0.1") {
		t.Fatalf("init pin wrote %q / %q", proxy.Status.InitResolvedDigest, proxy.Status.InitResolvedRef)
	}
	resolved := meta.FindStatusCondition(proxy.Status.Conditions, addonsv1alpha1.ConditionInitPackageResolved)
	if resolved == nil || resolved.Status != metav1.ConditionTrue {
		t.Fatalf("InitPackageResolved = %+v, want True", resolved)
	}

	secret := &corev1.Secret{}
	secretKey := types.NamespacedName{Namespace: ns, Name: "app-edge-1-deploy-spec"}
	if err := c.Get(t.Context(), secretKey, secret); err != nil {
		t.Fatalf("the deploy-spec Secret was not written: %v", err)
	}
	if _, ok := secret.Data[deployspec.FileName]; !ok {
		t.Fatal("the main spec went missing from the Secret")
	}
	doc := string(secret.Data[deployspec.InitFileName])
	if !strings.Contains(doc, "registry.internal/zarf/init@sha256:1217") {
		t.Fatalf("the init spec is not digest-pinned:\n%s", doc)
	}
	if !strings.Contains(doc, "edge-1-sc") {
		t.Fatalf("the init set entry did not render against the cluster:\n%s", doc)
	}
	if !strings.Contains(doc, "packageAPI: v1alpha1") {
		t.Fatalf("the init spec dropped the declared package API:\n%s", doc)
	}

	// An unrelated edit must not re-touch the registry for the pinned init package.
	initCalls := stub.calls["registry.internal/zarf/init"]
	beforeEdit := proxy.DeepCopy()
	proxy.Spec.DependsOn = []string{"something"}
	if err := c.Patch(t.Context(), proxy, client.MergeFrom(beforeEdit)); err != nil {
		t.Fatalf("editing the proxy: %v", err)
	}
	reconcile(t)
	if stub.calls["registry.internal/zarf/init"] != initCalls {
		t.Fatalf("an unrelated edit re-resolved the init package: %d calls", stub.calls["registry.internal/zarf/init"])
	}

	// A version bump re-resolves; an artifact that is not a ZarfInitConfig is refused.
	stub.results["registry.internal/zarf/init"] = resolver.Result{
		Digest:     "sha256:0bad",
		Name:       "not-init",
		APIVersion: "v1alpha1",
	}
	beforeBump := proxy.DeepCopy()
	proxy.Spec.Init.Package.Version = "v2.0.0"
	if err := c.Patch(t.Context(), proxy, client.MergeFrom(beforeBump)); err != nil {
		t.Fatalf("bumping the init version: %v", err)
	}
	reconcile(t)
	resolved = meta.FindStatusCondition(proxy.Status.Conditions, addonsv1alpha1.ConditionInitPackageResolved)
	if resolved == nil || resolved.Status != metav1.ConditionFalse ||
		resolved.Reason != addonsv1alpha1.ReasonNotAnInitPackage {
		t.Fatalf("InitPackageResolved = %+v, want False/%s", resolved, addonsv1alpha1.ReasonNotAnInitPackage)
	}

	// Un-declaring auto-init clears the pin and the condition, so no stale init Job input
	// survives the spec that asked for it.
	beforeDrop := proxy.DeepCopy()
	proxy.Spec.Init = nil
	if err := c.Patch(t.Context(), proxy, client.MergeFrom(beforeDrop)); err != nil {
		t.Fatalf("dropping init: %v", err)
	}
	reconcile(t)
	if proxy.Status.InitResolvedDigest != "" || proxy.Status.InitResolvedRef != "" {
		t.Fatalf("the init pin survived un-declaring auto-init: %q / %q",
			proxy.Status.InitResolvedDigest, proxy.Status.InitResolvedRef)
	}
	if meta.FindStatusCondition(proxy.Status.Conditions, addonsv1alpha1.ConditionInitPackageResolved) != nil {
		t.Fatal("InitPackageResolved survived un-declaring auto-init")
	}
}
