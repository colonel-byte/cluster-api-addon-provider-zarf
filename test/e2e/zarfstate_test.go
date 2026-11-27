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

package e2e

import (
	"context"
	"fmt"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"

	addonsv1alpha1 "github.com/colonel-byte/cluster-api-addon-provider-zarf/api/v1alpha1"
)

// `spec.package.fromZarfState` against a real Zarf deployment in the management cluster - the
// arrangement the field exists for, where the fleet pulls from the registry Zarf installed in
// the cluster the controller runs in. `e2e:up` zarf-initializes the management cluster in proxy
// mode, which is the only mode that yields `mtlsStrategy: zarf-managed` and therefore a
// registry CA in state, and copies the app fixture into that registry.
//
// Three things are true here that no other test in the suite covers: the credential comes from
// a Secret nobody authored, the CA comes from Zarf's own `zarf-registry-server-tls`, and the
// controller has to read *this* cluster's state rather than a workload cluster's - which is the
// opposite of every other Zarf-state read in the controller.
const (
	// The service address, not the one state records. Zarf records `127.0.0.1:5000` in proxy
	// mode - the per-node registry proxy's host port, which routes from nowhere a controller
	// pod runs - and issues the registry certificate for this name among others, so this is
	// both what resolves and what verifies.
	mgmtZarfRegistryHost  = "zarf-docker-registry.zarf.svc.cluster.local:5000"
	mgmtZarfAppRepository = mgmtZarfRegistryHost + "/packages/e2e-app"
)

// requireMgmtZarfState skips unless the management cluster carries Zarf state, so the suite
// still runs against a topology built before this step existed.
func requireMgmtZarfState(t *testing.T, c client.Client) {
	t.Helper()
	secret := &corev1.Secret{}
	err := c.Get(t.Context(), types.NamespacedName{Namespace: "zarf", Name: "zarf-state"}, secret)
	if apierrors.IsNotFound(err) {
		t.Skip("the management cluster is not zarf-initialized; `e2e:up` does that")
	}
	if err != nil {
		t.Fatalf("reading the management cluster's zarf state: %v", err)
	}
}

// TestFromZarfStateResolution is the cheap half: resolution alone, through a proxy that matches
// no cluster, so a wrong credential or an unverifiable certificate shows up in seconds.
func TestFromZarfStateResolution(t *testing.T) {
	c := phase1Client(t)
	requireMgmtZarfState(t, c)

	proxy := &addonsv1alpha1.ZarfPackageProxy{
		ObjectMeta: metav1.ObjectMeta{Name: "zarf-state-resolve", Namespace: "default"},
		Spec: addonsv1alpha1.ZarfPackageProxySpec{
			ClusterSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{"caapz.e2e/registry-auth": "no-cluster"},
			},
			Package: &addonsv1alpha1.ZarfPackageSource{
				Repository: mgmtZarfAppRepository,
				Version:    appPackageVersion,
				// The whole test: no pullSecretRef, no caBundleSecretRef. Both come from the
				// management cluster's own Zarf state.
				FromZarfState: true,
			},
		},
	}
	deleteProxy(t, c, proxy)
	if err := c.Create(t.Context(), proxy); err != nil {
		t.Fatalf("creating the proxy: %v", err)
	}
	t.Cleanup(func() {
		_ = c.Delete(context.Background(), proxy)
	})

	condition := waitResolution(t, c, proxy)
	if condition.Status != metav1.ConditionTrue {
		t.Fatalf("want the package resolved from zarf state, got %s/%s: %s",
			condition.Status, condition.Reason, condition.Message)
	}

	current := &addonsv1alpha1.ZarfPackageProxy{}
	if err := c.Get(t.Context(), client.ObjectKeyFromObject(proxy), current); err != nil {
		t.Fatalf("re-reading the proxy: %v", err)
	}
	if current.Status.PackageName != appPackageName {
		t.Fatalf("want the %s package, got %q", appPackageName, current.Status.PackageName)
	}
}

// TestFromZarfStateDeploy is the half that needs the pod: the credential and the CA the
// controller read out of Zarf state have to travel into the deploy Job's mount, or the pull
// fails there having succeeded in the controller.
func TestFromZarfStateDeploy(t *testing.T) {
	c := phase1Client(t)
	workload := workloadClient(t)
	requireMgmtZarfState(t, c)

	// The init-first gate holds an ordinary package on a cluster with no zarf state.
	if err := workload.Get(t.Context(),
		types.NamespacedName{Namespace: "zarf", Name: "zarf-state"}, &corev1.Secret{}); err != nil {
		t.Skip("the workload cluster is not zarf-initialized; TestAutoInitAndDeploy establishes that")
	}

	// An earlier test's deploy of the same fixture would make this pass without deploying
	// anything, so start from the ConfigMap not being there.
	existing := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: appPackageName, Namespace: "default"},
	}
	if err := workload.Delete(t.Context(), existing); err != nil && !apierrors.IsNotFound(err) {
		t.Fatalf("clearing the fixture ConfigMap: %v", err)
	}
	if err := wait.PollUntilContextTimeout(t.Context(), 2*time.Second, time.Minute, true,
		func(ctx context.Context) (bool, error) {
			err := workload.Get(ctx, client.ObjectKeyFromObject(existing), &corev1.ConfigMap{})
			return apierrors.IsNotFound(err), nil
		}); err != nil {
		t.Fatalf("the fixture ConfigMap never went away: %v", err)
	}

	proxy := &addonsv1alpha1.ZarfPackageProxy{
		ObjectMeta: metav1.ObjectMeta{Name: "zarf-state-deploy", Namespace: "default"},
		Spec: addonsv1alpha1.ZarfPackageProxySpec{
			ClusterSelector: metav1.LabelSelector{},
			Package: &addonsv1alpha1.ZarfPackageSource{
				Repository:    mgmtZarfAppRepository,
				Version:       appPackageVersion,
				FromZarfState: true,
			},
		},
	}
	deleteProxy(t, c, proxy)
	if err := c.Create(t.Context(), proxy); err != nil {
		t.Fatalf("creating the proxy: %v", err)
	}
	t.Cleanup(func() {
		_ = c.Delete(context.Background(), proxy)
	})

	key := types.NamespacedName{Namespace: "default", Name: proxy.Name + "-" + workloadCluster}
	waitLong(t, "the deploy to succeed from the management cluster's zarf registry", func(ctx context.Context) (bool, error) {
		phase, message, err := releasePhase(ctx, c, key)
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		switch phase {
		case string(addonsv1alpha1.PhaseDeployed):
			return true, nil
		case string(addonsv1alpha1.PhaseFailed):
			return false, fmt.Errorf("the release failed: %s", message)
		}
		return false, nil
	})

	deployed := &corev1.ConfigMap{}
	if err := workload.Get(t.Context(), client.ObjectKeyFromObject(existing), deployed); err != nil {
		t.Fatalf("the fixture ConfigMap is not on the workload cluster: %v", err)
	}
	if deployed.Data["marker"] != appPackageName {
		t.Fatalf("want marker %q, got %q", appPackageName, deployed.Data["marker"])
	}

	// The deploy-spec Secret is where the pod's half comes from, and it is worth asserting
	// directly: a deploy that happened to succeed without the mounted CA would be a different
	// bug wearing this test's result.
	spec := &corev1.Secret{}
	if err := c.Get(t.Context(),
		types.NamespacedName{Namespace: "default", Name: key.Name + "-deploy-spec"}, spec); err != nil {
		t.Fatalf("reading the deploy-spec secret: %v", err)
	}
	if len(spec.Data["config.json"]) == 0 {
		t.Error("want the registry credentials in the deploy-spec secret")
	}
	if len(spec.Data["ca.crt"]) == 0 {
		t.Error("want the zarf registry CA in the deploy-spec secret")
	}
	if !meta.IsStatusConditionTrue(mustRelease(t, c, key).Status.Conditions, addonsv1alpha1.ConditionDeploySpecReady) {
		t.Error("want DeploySpecReady on the release")
	}
}

func mustRelease(t *testing.T, c client.Client, key types.NamespacedName) *addonsv1alpha1.ZarfPackageRelease {
	t.Helper()
	release := &addonsv1alpha1.ZarfPackageRelease{}
	if err := c.Get(t.Context(), key, release); err != nil {
		t.Fatalf("reading the release: %v", err)
	}
	return release
}
