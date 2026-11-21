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
	"os"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"

	addonsv1alpha1 "github.com/colonel-byte/cluster-api-addon-provider-zarf/api/v1alpha1"
	"github.com/colonel-byte/cluster-api-addon-provider-zarf/internal/envtest"
)

func TestMain(m *testing.M) {
	os.Exit(envtest.Run(m))
}

// TestEnvtestHarness proves the harness itself: both this provider's CRDs and Cluster API's
// install, their CEL rules run, and a Cluster round-trips through the real API server.
func TestEnvtestHarness(t *testing.T) {
	c := envtest.Client(t)
	ns := envtest.Namespace(t, c)

	cluster := &clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: "workload-1", Namespace: ns},
		// spec carries omitzero and the CRD requires it, so one field has to be set.
		Spec: clusterv1.ClusterSpec{Paused: ptr.To(false)},
	}
	if err := c.Create(t.Context(), cluster); err != nil {
		t.Fatalf("unable to create a Cluster: %v", err)
	}

	proxy := &addonsv1alpha1.ZarfPackageProxy{
		ObjectMeta: metav1.ObjectMeta{Name: "podinfo", Namespace: ns},
		Spec: addonsv1alpha1.ZarfPackageProxySpec{
			ClusterSelector: metav1.LabelSelector{},
			Package: addonsv1alpha1.ZarfPackageSource{
				Repository: "registry.internal/zarf/podinfo",
				Version:    "6.5.0",
			},
		},
	}
	if err := c.Create(t.Context(), proxy); err != nil {
		t.Fatalf("unable to create a ZarfPackageProxy: %v", err)
	}

	// The spec.set CEL rule must reject at admission, not at deploy time.
	rejected := proxy.DeepCopy()
	rejected.Name = "podinfo-set"
	rejected.Spec.Set = map[string]string{"KEY": "value"}
	if err := c.Create(t.Context(), rejected); err == nil {
		t.Fatal("expected the spec.set CEL rule to reject a proxy without spec.package.api: v1alpha1")
	}
}
