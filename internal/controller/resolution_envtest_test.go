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
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	addonsv1alpha1 "github.com/colonel-byte/cluster-api-addon-provider-zarf/api/v1alpha1"
	"github.com/colonel-byte/cluster-api-addon-provider-zarf/internal/envtest"
	"github.com/colonel-byte/cluster-api-addon-provider-zarf/internal/resolver"
)

// stubResolver is the registry stand-in: canned results, call counts, and a movable tag.
type stubResolver struct {
	result       resolver.Result
	err          error
	tagDigest    string
	resolveCalls int
	digestCalls  int
	lastOpts     resolver.Options
}

func (s *stubResolver) Resolve(_ context.Context, _, _ string, opts resolver.Options) (resolver.Result, error) {
	s.resolveCalls++
	s.lastOpts = opts
	return s.result, s.err
}

func (s *stubResolver) ResolveDigest(_ context.Context, _, _ string, opts resolver.Options) (string, error) {
	s.digestCalls++
	s.lastOpts = opts
	return s.tagDigest, nil
}

// TestResolution walks the §6 step 4 lifecycle against a real API server with a stubbed
// registry: pin on first reconcile, no re-pin on an unrelated edit, drift warning on a moved
// tag, re-pin on a version bump.
func TestResolution(t *testing.T) {
	c := envtest.Client(t)
	ns := envtest.Namespace(t, c)
	newTestProxy(t, c, ns, "resolved", nil)

	stub := &stubResolver{
		result: resolver.Result{
			Digest:     "sha256:aaaa",
			Name:       "resolved",
			APIVersion: "v1beta1",
		},
		tagDigest: "sha256:aaaa",
	}
	r := &ZarfPackageProxyReconciler{Client: c, Resolver: stub}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "resolved"}}
	proxy := &addonsv1alpha1.ZarfPackageProxy{}
	reconcile := func(t *testing.T) {
		t.Helper()
		if _, err := r.Reconcile(t.Context(), req); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		if err := c.Get(t.Context(), req.NamespacedName, proxy); err != nil {
			t.Fatalf("re-reading the proxy: %v", err)
		}
	}

	reconcile(t)
	if proxy.Status.ResolvedDigest != "sha256:aaaa" {
		t.Fatalf("resolvedDigest = %q, want sha256:aaaa", proxy.Status.ResolvedDigest)
	}
	if proxy.Status.PackageKind != "ZarfPackageConfig" {
		t.Fatalf("packageKind = %q, want ZarfPackageConfig", proxy.Status.PackageKind)
	}
	resolved := meta.FindStatusCondition(proxy.Status.Conditions, addonsv1alpha1.ConditionPackageResolved)
	if resolved == nil || resolved.Status != metav1.ConditionTrue {
		t.Fatalf("PackageResolved = %+v, want True", resolved)
	}
	if meta.FindStatusCondition(proxy.Status.Conditions, addonsv1alpha1.ConditionPackageSignatureVerified) != nil {
		t.Fatal("PackageSignatureVerified must be absent with no key named")
	}
	if stub.resolveCalls != 1 {
		t.Fatalf("resolveCalls = %d, want 1", stub.resolveCalls)
	}

	// An unrelated spec edit must not re-resolve - only the drift check runs.
	beforeEdit := proxy.DeepCopy()
	proxy.Spec.DependsOn = proxyRefs("something")
	if err := c.Patch(t.Context(), proxy, client.MergeFrom(beforeEdit)); err != nil {
		t.Fatalf("editing the proxy: %v", err)
	}
	reconcile(t)
	if stub.resolveCalls != 1 {
		t.Fatalf("an unrelated edit re-resolved: resolveCalls = %d, want 1", stub.resolveCalls)
	}
	if stub.digestCalls == 0 {
		t.Fatal("the drift check never ran")
	}
	if meta.FindStatusCondition(proxy.Status.Conditions, addonsv1alpha1.ConditionTagDrift) != nil {
		t.Fatal("TagDrift set while the tag still matches")
	}

	// The registry's tag moves: a warning, never an adoption.
	stub.tagDigest = "sha256:bbbb"
	reconcile(t)
	drift := meta.FindStatusCondition(proxy.Status.Conditions, addonsv1alpha1.ConditionTagDrift)
	if drift == nil || drift.Reason != addonsv1alpha1.ReasonTagContentChanged {
		t.Fatalf("TagDrift = %+v, want reason %s", drift, addonsv1alpha1.ReasonTagContentChanged)
	}
	if proxy.Status.ResolvedDigest != "sha256:aaaa" {
		t.Fatalf("drift adopted the new digest: %q", proxy.Status.ResolvedDigest)
	}

	// A version bump is the update path: re-resolve, re-pin, drift cleared.
	stub.result.Digest = "sha256:bbbb"
	beforeBump := proxy.DeepCopy()
	proxy.Spec.Package.Version = "2.0.0"
	if err := c.Patch(t.Context(), proxy, client.MergeFrom(beforeBump)); err != nil {
		t.Fatalf("bumping the version: %v", err)
	}
	reconcile(t)
	if stub.resolveCalls != 2 {
		t.Fatalf("the version bump did not re-resolve: resolveCalls = %d", stub.resolveCalls)
	}
	if proxy.Status.ResolvedDigest != "sha256:bbbb" || !strings.HasSuffix(proxy.Status.ResolvedRef, ":2.0.0") {
		t.Fatalf("re-pin wrote %q / %q", proxy.Status.ResolvedDigest, proxy.Status.ResolvedRef)
	}
	if meta.FindStatusCondition(proxy.Status.Conditions, addonsv1alpha1.ConditionTagDrift) != nil {
		t.Fatal("TagDrift survived a re-pin")
	}
}

// TestResolutionSignatureKey covers the key path: the secret's cosign.pub reaches the resolver,
// verification surfaces as PackageSignatureVerified, a cross-namespace ref is refused with
// CrossNamespaceNotEnabled, and an ApplyOnce proxy never re-touches the registry once pinned.
func TestResolutionSignatureKey(t *testing.T) {
	c := envtest.Client(t)
	ns := envtest.Namespace(t, c)

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "signing-key", Namespace: ns},
		Data:       map[string][]byte{"cosign.pub": []byte("PEM")},
	}
	if err := c.Create(t.Context(), secret); err != nil {
		t.Fatalf("creating the key secret: %v", err)
	}

	proxy := &addonsv1alpha1.ZarfPackageProxy{
		ObjectMeta: metav1.ObjectMeta{Name: "signed", Namespace: ns},
		Spec: addonsv1alpha1.ZarfPackageProxySpec{
			ClusterSelector: metav1.LabelSelector{MatchLabels: map[string]string{"tier": "edge"}},
			Strategy:        addonsv1alpha1.StrategyApplyOnce,
			Package: addonsv1alpha1.ZarfPackageSource{
				Repository:            "registry.internal/zarf/signed",
				Version:               "1.0.0",
				SignatureKeySecretRef: &addonsv1alpha1.NamespacedReference{Name: "signing-key"},
			},
		},
	}
	if err := c.Create(t.Context(), proxy); err != nil {
		t.Fatalf("creating the proxy: %v", err)
	}

	stub := &stubResolver{
		result: resolver.Result{
			Digest:     "sha256:cccc",
			Verified:   true,
			APIVersion: "v1beta1",
		},
		tagDigest: "sha256:cccc",
	}
	r := &ZarfPackageProxyReconciler{Client: c, Resolver: stub}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "signed"}}
	if _, err := r.Reconcile(t.Context(), req); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if err := c.Get(t.Context(), req.NamespacedName, proxy); err != nil {
		t.Fatalf("re-reading the proxy: %v", err)
	}

	if string(stub.lastOpts.CosignPublicKey) != "PEM" {
		t.Fatalf("the secret's key never reached the resolver: %q", stub.lastOpts.CosignPublicKey)
	}
	verified := meta.FindStatusCondition(proxy.Status.Conditions, addonsv1alpha1.ConditionPackageSignatureVerified)
	if verified == nil || verified.Status != metav1.ConditionTrue {
		t.Fatalf("PackageSignatureVerified = %+v, want True", verified)
	}

	// ApplyOnce, pinned: no further registry traffic of any kind.
	calls := stub.resolveCalls + stub.digestCalls
	if _, err := r.Reconcile(t.Context(), req); err != nil {
		t.Fatalf("second Reconcile: %v", err)
	}
	if stub.resolveCalls+stub.digestCalls != calls {
		t.Fatal("an ApplyOnce proxy re-touched the registry after pinning")
	}

	// A cross-namespace key without --secret-namespaces is refused, not resolved elsewhere.
	other := envtest.Namespace(t, c)
	beforeRef := proxy.DeepCopy()
	proxy.Spec.Package.SignatureKeySecretRef = &addonsv1alpha1.NamespacedReference{Name: "signing-key", Namespace: other}
	proxy.Spec.Package.Version = "2.0.0"
	if err := c.Patch(t.Context(), proxy, client.MergeFrom(beforeRef)); err != nil {
		t.Fatalf("re-pointing the key: %v", err)
	}
	if _, err := r.Reconcile(t.Context(), req); err != nil {
		t.Fatalf("third Reconcile: %v", err)
	}
	if err := c.Get(t.Context(), req.NamespacedName, proxy); err != nil {
		t.Fatalf("re-reading the proxy: %v", err)
	}
	resolved := meta.FindStatusCondition(proxy.Status.Conditions, addonsv1alpha1.ConditionPackageResolved)
	if resolved == nil || resolved.Status != metav1.ConditionFalse ||
		resolved.Reason != addonsv1alpha1.ReasonCrossNamespaceNotEnabled {
		t.Fatalf("PackageResolved = %+v, want False/%s", resolved, addonsv1alpha1.ReasonCrossNamespaceNotEnabled)
	}
}
