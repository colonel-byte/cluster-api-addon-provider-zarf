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
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
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

// TestDeploySpecSecret drives the full reconcile with a stubbed registry and checks the
// per-release Secret: digest-pinned package reference, rendered {{ .Cluster }} values,
// release-owned for garbage collection, and the DeploySpecReady condition on the release.
func TestDeploySpecSecret(t *testing.T) {
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
			Package: &addonsv1alpha1.ZarfPackageSource{
				Repository: "registry.internal/zarf/app",
				Version:    "1.0.0",
			},
			Components: []addonsv1alpha1.ComponentSelector{
				{
					Name: "core",
				},
			},
			Values: &apiextensionsv1.JSON{Raw: []byte(`{"clusterName":"{{ .Cluster.metadata.name }}"}`)},
		},
	}
	if err := c.Create(t.Context(), proxy); err != nil {
		t.Fatalf("creating the proxy: %v", err)
	}

	stub := &stubResolver{
		result: resolver.Result{
			Digest:     "sha256:feed",
			APIVersion: "v1beta1",
		},
		tagDigest: "sha256:feed",
	}
	r := &ZarfPackageProxyReconciler{Client: c, Resolver: stub}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "app"}}
	if _, err := r.Reconcile(t.Context(), req); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	// The first pass stamps the release and pins the digest; the second writes the spec.
	if _, err := r.Reconcile(t.Context(), req); err != nil {
		t.Fatalf("second Reconcile: %v", err)
	}

	secret := &corev1.Secret{}
	secretKey := types.NamespacedName{Namespace: ns, Name: "app-edge-1-deploy-spec"}
	if err := c.Get(t.Context(), secretKey, secret); err != nil {
		t.Fatalf("the deploy-spec Secret was not written: %v", err)
	}
	doc := string(secret.Data[deployspec.FileName])
	if !strings.Contains(doc, "registry.internal/zarf/app@sha256:feed") {
		t.Fatalf("the spec is not digest-pinned:\n%s", doc)
	}
	if strings.Contains(doc, ":1.0.0") {
		t.Fatalf("a tag leaked into the spec:\n%s", doc)
	}
	if !strings.Contains(doc, `clusterName\":\"edge-1`) && !strings.Contains(doc, `clusterName":"edge-1`) {
		t.Fatalf("the cluster name did not render into values:\n%s", doc)
	}

	owner := metav1.GetControllerOf(secret)
	if owner == nil || owner.Kind != "ZarfPackageRelease" || owner.Name != "app-edge-1" {
		t.Fatalf("secret controller owner = %+v, want the release", owner)
	}

	release := &addonsv1alpha1.ZarfPackageRelease{}
	if err := c.Get(t.Context(), types.NamespacedName{Namespace: ns, Name: "app-edge-1"}, release); err != nil {
		t.Fatalf("re-reading the release: %v", err)
	}
	ready := meta.FindStatusCondition(release.Status.Conditions, addonsv1alpha1.ConditionDeploySpecReady)
	if ready == nil || ready.Status != metav1.ConditionTrue {
		t.Fatalf("DeploySpecReady = %+v, want True", ready)
	}

	// A template naming a field the cluster does not carry refuses with RenderFailed, and the
	// stale Secret keeps the last good rendering.
	beforeValues := proxy.DeepCopy()
	proxy.Spec.Values = &apiextensionsv1.JSON{Raw: []byte(`{"x":"{{ .Cluster.metadata.nope }}"}`)}
	if err := c.Patch(t.Context(), proxy, client.MergeFrom(beforeValues)); err != nil {
		t.Fatalf("breaking the template: %v", err)
	}
	if _, err := r.Reconcile(t.Context(), req); err != nil {
		t.Fatalf("third Reconcile: %v", err)
	}
	if err := c.Get(t.Context(), types.NamespacedName{Namespace: ns, Name: "app-edge-1"}, release); err != nil {
		t.Fatalf("re-reading the release: %v", err)
	}
	ready = meta.FindStatusCondition(release.Status.Conditions, addonsv1alpha1.ConditionDeploySpecReady)
	if ready == nil || ready.Status != metav1.ConditionFalse || ready.Reason != addonsv1alpha1.ReasonRenderFailed {
		t.Fatalf("DeploySpecReady = %+v, want False/%s", ready, addonsv1alpha1.ReasonRenderFailed)
	}
}

// TestStagedDeploySpecDocuments is the staged form's rendering contract: a four-stage proxy
// writes one document per stage into the one Secret, and editing one stage's values changes
// exactly that stage's document.
func TestStagedDeploySpecDocuments(t *testing.T) {
	c := envtest.Client(t)
	ns := envtest.Namespace(t, c)

	cluster := &clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: "staged-1", Namespace: ns, Labels: map[string]string{"tier": "staged"}},
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
		ObjectMeta: metav1.ObjectMeta{Name: "boot", Namespace: ns},
		Spec: addonsv1alpha1.ZarfPackageProxySpec{
			ClusterSelector: metav1.LabelSelector{MatchLabels: map[string]string{"tier": "staged"}},
			DeletionPolicy:  addonsv1alpha1.PolicyRetain,
			Packages: map[string]addonsv1alpha1.ZarfPackageSource{
				"a": {Repository: "registry.internal/zarf/a", Version: "1.0.0"},
				"b": {Repository: "registry.internal/zarf/b", Version: "2.0.0"},
			},
			Stages: []addonsv1alpha1.PackageStage{
				{Name: "a-seed", PackageRef: "a", Strategy: addonsv1alpha1.StrategyApplyOnce},
				{Name: "b-seed", PackageRef: "b", Strategy: addonsv1alpha1.StrategyApplyOnce},
				{Name: "a-final", PackageRef: "a"},
				{Name: "b-final", PackageRef: "b"},
			},
		},
	}
	if err := c.Create(t.Context(), proxy); err != nil {
		t.Fatalf("creating the staged proxy: %v", err)
	}

	stub := &stubResolver{
		result: resolver.Result{
			Digest:     "sha256:feed",
			APIVersion: "v1beta1",
		},
		tagDigest: "sha256:feed",
	}
	r := &ZarfPackageProxyReconciler{Client: c, Resolver: stub}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "boot"}}
	for range 2 {
		if _, err := r.Reconcile(t.Context(), req); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
	}

	secret := &corev1.Secret{}
	secretKey := types.NamespacedName{Namespace: ns, Name: "boot-staged-1-deploy-spec"}
	if err := c.Get(t.Context(), secretKey, secret); err != nil {
		t.Fatalf("the deploy-spec Secret was not written: %v", err)
	}
	wantFiles := []string{"stage-a-seed.yaml", "stage-b-seed.yaml", "stage-a-final.yaml", "stage-b-final.yaml"}
	for _, name := range wantFiles {
		if len(secret.Data[name]) == 0 {
			t.Fatalf("stage document %s missing; secret holds %d files", name, len(secret.Data))
		}
	}
	if !strings.Contains(string(secret.Data["stage-a-seed.yaml"]), "registry.internal/zarf/a@sha256:feed") {
		t.Fatalf("stage document is not digest-pinned:\n%s", secret.Data["stage-a-seed.yaml"])
	}
	kept := map[string][]byte{}
	for name, data := range secret.Data {
		kept[name] = append([]byte(nil), data...)
	}

	// Edit one stage's values: exactly that document changes.
	fresh := &addonsv1alpha1.ZarfPackageProxy{}
	if err := c.Get(t.Context(), types.NamespacedName{Namespace: ns, Name: "boot"}, fresh); err != nil {
		t.Fatalf("re-reading the proxy: %v", err)
	}
	fresh.Spec.Stages[3].Values = &apiextensionsv1.JSON{Raw: []byte(`{"marker":"edited"}`)}
	if err := c.Update(t.Context(), fresh); err != nil {
		t.Fatalf("editing stage 4's values: %v", err)
	}
	if _, err := r.Reconcile(t.Context(), req); err != nil {
		t.Fatalf("Reconcile after the edit: %v", err)
	}
	if err := c.Get(t.Context(), secretKey, secret); err != nil {
		t.Fatalf("re-reading the Secret: %v", err)
	}
	for name := range kept {
		changed := !strings.EqualFold(string(kept[name]), string(secret.Data[name]))
		if name == "stage-b-final.yaml" && !changed {
			t.Fatalf("the edited stage's document did not change")
		}
		if name != "stage-b-final.yaml" && changed {
			t.Fatalf("document %s changed on another stage's edit", name)
		}
	}
}
