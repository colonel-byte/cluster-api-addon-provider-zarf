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
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/colonel-byte/cluster-api-addon-provider-zarf/internal/envtest"
)

// TestWorkloadProbes drives workloadClient and both probes against the envtest server playing
// its own workload cluster: the kubeconfig secret carries the test server's own connection.
func TestWorkloadProbes(t *testing.T) {
	c := envtest.Client(t)
	ns := envtest.Namespace(t, c)
	ctx := t.Context()
	r := &ZarfPackageProxyReconciler{Client: c}

	t.Run("refuses a cluster with no kubeconfig secret", func(t *testing.T) {
		if _, err := r.workloadClient(ctx, ns, "absent"); err == nil {
			t.Fatal("want an error for a missing kubeconfig secret")
		}
	})

	kubeconfigSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "workload-1-kubeconfig", Namespace: ns},
		Data:       map[string][]byte{"wrong-key": []byte("x")},
	}
	if err := c.Create(ctx, kubeconfigSecret); err != nil {
		t.Fatalf("creating the kubeconfig secret: %v", err)
	}

	t.Run("refuses a secret without the CAPI value key", func(t *testing.T) {
		if _, err := r.workloadClient(ctx, ns, "workload-1"); err == nil {
			t.Fatal("want an error for a secret missing the value key")
		}
	})

	before := kubeconfigSecret.DeepCopy()
	kubeconfigSecret.Data = map[string][]byte{kubeconfigSecretKey: envtest.Kubeconfig(t)}
	if err := c.Patch(ctx, kubeconfigSecret, client.MergeFrom(before)); err != nil {
		t.Fatalf("patching the kubeconfig secret: %v", err)
	}
	workload, err := r.workloadClient(ctx, ns, "workload-1")
	if err != nil {
		t.Fatalf("building the workload client: %v", err)
	}

	// The zarf namespace is cluster-scoped state shared by every test in this binary; treat an
	// AlreadyExists from a parallel binary run as created.
	zarfNS := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: zarfstate.ZarfNamespaceName}}
	if err := c.Create(ctx, zarfNS); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("creating the zarf namespace: %v", err)
	}

	t.Run("probes report absent state and record", func(t *testing.T) {
		if got, err := hasZarfState(ctx, workload); err != nil || got {
			t.Fatalf("hasZarfState = %v, %v; want false, nil", got, err)
		}
		if got, err := hasDeployedPackage(ctx, workload, "podinfo"); err != nil || got {
			t.Fatalf("hasDeployedPackage = %v, %v; want false, nil", got, err)
		}
	})

	for _, name := range []string{zarfstate.ZarfStateSecretName, zarfconfig.ZarfPackagePrefix + "podinfo"} {
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: zarfstate.ZarfNamespaceName},
		}
		if err := c.Create(ctx, secret); err != nil {
			t.Fatalf("creating %s: %v", name, err)
		}
		t.Cleanup(func() {
			// t.Context is already canceled in cleanups; the shared secret must still go.
			_ = c.Delete(context.Background(), secret) //nolint:errcheck // best-effort cleanup
		})
	}

	t.Run("probes report present state and record", func(t *testing.T) {
		if got, err := hasZarfState(ctx, workload); err != nil || !got {
			t.Fatalf("hasZarfState = %v, %v; want true, nil", got, err)
		}
		if got, err := hasDeployedPackage(ctx, workload, "podinfo"); err != nil || !got {
			t.Fatalf("hasDeployedPackage = %v, %v; want true, nil", got, err)
		}
		if got, err := hasDeployedPackage(ctx, workload, "other"); err != nil || got {
			t.Fatalf("hasDeployedPackage(other) = %v, %v; want false, nil", got, err)
		}
	})
}
