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

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	addonsv1alpha1 "github.com/colonel-byte/cluster-api-addon-provider-zarf/api/v1alpha1"
	"github.com/colonel-byte/cluster-api-addon-provider-zarf/internal/envtest"
)

// TestReleaseToProxiesFanOut checks §5B's dependent fan-out: a release event enqueues its own
// proxy and every proxy whose dependsOn names it, and nothing else.
func TestReleaseToProxiesFanOut(t *testing.T) {
	c := envtest.Client(t)
	ns := envtest.Namespace(t, c)

	newTestProxy(t, c, ns, "csi", nil)
	newTestProxy(t, c, ns, "logging", []string{"csi"})
	newTestProxy(t, c, ns, "metrics", []string{"csi"})
	newTestProxy(t, c, ns, "unrelated", []string{"something-else"})

	release := &addonsv1alpha1.ZarfPackageRelease{
		ObjectMeta: metav1.ObjectMeta{Name: "csi-edge-1", Namespace: ns},
		Spec: addonsv1alpha1.ZarfPackageReleaseSpec{
			ClusterRef:      corev1.LocalObjectReference{Name: "edge-1"},
			PackageProxyRef: addonsv1alpha1.NamespacedReference{Name: "csi"},
		},
	}

	r := &ZarfPackageProxyReconciler{Client: c}
	got := r.releaseToProxies(t.Context(), release)

	want := map[reconcile.Request]bool{
		{NamespacedName: types.NamespacedName{Namespace: ns, Name: "csi"}}:     true,
		{NamespacedName: types.NamespacedName{Namespace: ns, Name: "logging"}}: true,
		{NamespacedName: types.NamespacedName{Namespace: ns, Name: "metrics"}}: true,
	}
	if len(got) != len(want) {
		t.Fatalf("got %d requests (%v), want %d", len(got), got, len(want))
	}
	for _, req := range got {
		if !want[req] {
			t.Errorf("unexpected request %v", req)
		}
	}
}
