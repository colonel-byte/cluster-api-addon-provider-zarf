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
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"

	addonsv1alpha1 "github.com/colonel-byte/cluster-api-addon-provider-zarf/api/v1alpha1"
)

// Phase 3 of dist/e2e-test-plan.md, uninstall half: what happens to a deployed package when its
// declaration goes away (architecture.md §8). Every path here had only envtest coverage before,
// which means the removal Job had never run a pod - envtest has no kubelet - so nothing had
// proven that `packager.Remove` works against a real cluster or that the finalizer dance
// terminates rather than wedging a release.

// deployedPackageRecordName is zarf's per-package record on the workload cluster: the thing §3's
// adoption reads and §8's removal is judged by.
func deployedPackageRecordName(packageName string) types.NamespacedName {
	return types.NamespacedName{Namespace: "zarf", Name: "zarf-package-" + packageName}
}

// deployProxy creates a proxy for the app fixture and waits for its release to deploy, which is
// the starting state every test in this file needs. policy selects the deletion behaviour under
// test; name keeps the proxies (and so the releases and Jobs) distinct.
func deployProxy(t *testing.T, c client.Client, name string, policy addonsv1alpha1.DeletionPolicy, strategy addonsv1alpha1.DeploymentStrategy) *addonsv1alpha1.ZarfPackageProxy {
	t.Helper()
	proxy := &addonsv1alpha1.ZarfPackageProxy{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: addonsv1alpha1.ZarfPackageProxySpec{
			ClusterSelector: metav1.LabelSelector{},
			DeletionPolicy:  policy,
			Strategy:        strategy,
			Package: addonsv1alpha1.ZarfPackageSource{
				Repository: appPackageRepository,
				Version:    appPackageVersion,
				API:        addonsv1alpha1.PackageAPIV1Beta1,
			},
		},
	}
	deleteProxy(t, c, proxy)
	if err := c.Create(t.Context(), proxy); err != nil {
		t.Fatalf("creating proxy %s: %v", name, err)
	}

	releaseKey := types.NamespacedName{Namespace: "default", Name: name + "-" + workloadCluster}
	want := addonsv1alpha1.PhaseDeployed
	if strategy == addonsv1alpha1.StrategyApplyOnce {
		want = addonsv1alpha1.PhaseCompleted
	}
	waitLong(t, "the release never finished deploying", func(ctx context.Context) (bool, error) {
		phase, reason, err := releasePhase(ctx, c, releaseKey)
		if err != nil {
			return false, client.IgnoreNotFound(err)
		}
		if reason == addonsv1alpha1.ReasonDeployFailed {
			return false, terminalFailure(ctx, c, releaseKey, "the deploy Job failed")
		}
		t.Logf("waiting for %s to deploy: phase=%s reason=%s", name, phase, reason)
		return phase == string(want), nil
	})
	return proxy
}

// TestDeletionPolicyDelete is §8A's Delete policy end to end: deleting the proxy runs a removal
// Job that uninstalls the package from the workload cluster, and only then does the release -
// and with it the proxy - go away. The removal Job actually running is the point; envtest never
// started one.
func TestDeletionPolicyDelete(t *testing.T) {
	c := phase1Client(t)
	workload := workloadClient(t)

	proxy := deployProxy(t, c, "e2e-remove", addonsv1alpha1.PolicyDelete, addonsv1alpha1.StrategyReconcile)
	t.Cleanup(func() {
		_ = c.Delete(context.Background(), proxy) //nolint:errcheck // best-effort cleanup
	})

	// The package is on the cluster, and zarf has recorded it under the resolved package name.
	if err := c.Get(t.Context(), client.ObjectKeyFromObject(proxy), proxy); err != nil {
		t.Fatalf("re-reading the proxy: %v", err)
	}
	packageName := proxy.Status.PackageName
	if packageName == "" {
		t.Fatal("the proxy recorded no packageName, so removal has nothing to remove by")
	}
	if err := workload.Get(t.Context(), deployedPackageRecordName(packageName), &corev1.Secret{}); err != nil {
		t.Fatalf("zarf left no deployed-package record to remove: %v", err)
	}

	// Deleting the declaration is what owes the uninstall.
	if err := c.Delete(t.Context(), proxy); err != nil {
		t.Fatalf("deleting the proxy: %v", err)
	}

	// The release must finalize - if the removal Job wedges, this is where it shows.
	releaseKey := types.NamespacedName{Namespace: "default", Name: "e2e-remove-" + workloadCluster}
	waitLong(t, "the release never finalized after proxy deletion", func(ctx context.Context) (bool, error) {
		release := &addonsv1alpha1.ZarfPackageRelease{}
		err := c.Get(ctx, releaseKey, release)
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		if err != nil {
			return false, err
		}
		if failed := release.Status.Conditions; failed != nil {
			for _, cond := range failed {
				if cond.Type == addonsv1alpha1.ConditionRemovalFailed && cond.Status == metav1.ConditionTrue {
					t.Logf("removal reported a failure and is retrying: %s", cond.Message)
				}
			}
		}
		t.Logf("waiting for the release to finalize: phase=%s", release.Status.Phase)
		return false, nil
	})

	// And the proxy itself, whose own finalizer waits for its releases.
	waitLong(t, "the proxy never finalized", func(ctx context.Context) (bool, error) {
		err := c.Get(ctx, client.ObjectKeyFromObject(proxy), &addonsv1alpha1.ZarfPackageProxy{})
		return apierrors.IsNotFound(err), client.IgnoreNotFound(err)
	})

	// The uninstall reached the cluster: both zarf's record and the package's own content.
	if err := workload.Get(t.Context(), deployedPackageRecordName(packageName), &corev1.Secret{}); !apierrors.IsNotFound(err) {
		t.Fatalf("zarf's deployed-package record survived the removal (err=%v)", err)
	}
	if err := workload.Get(t.Context(),
		types.NamespacedName{Namespace: "default", Name: appPackageName}, &corev1.ConfigMap{}); !apierrors.IsNotFound(err) {
		t.Fatalf("the package's ConfigMap survived the removal (err=%v)", err)
	}
}

// TestDeletionPolicyRetain is the other half of §8A: Retain orphans the deployed package on
// purpose. The release carries no removal finalizer at all, so its deletion cannot block on
// anything, and what was deployed keeps running with nothing tracking it.
func TestDeletionPolicyRetain(t *testing.T) {
	c := phase1Client(t)
	workload := workloadClient(t)

	proxy := deployProxy(t, c, "e2e-retain", addonsv1alpha1.PolicyRetain, addonsv1alpha1.StrategyReconcile)
	t.Cleanup(func() {
		_ = c.Delete(context.Background(), proxy) //nolint:errcheck // best-effort cleanup
	})
	if err := c.Get(t.Context(), client.ObjectKeyFromObject(proxy), proxy); err != nil {
		t.Fatalf("re-reading the proxy: %v", err)
	}
	packageName := proxy.Status.PackageName

	releaseKey := types.NamespacedName{Namespace: "default", Name: "e2e-retain-" + workloadCluster}
	release := &addonsv1alpha1.ZarfPackageRelease{}
	if err := c.Get(t.Context(), releaseKey, release); err != nil {
		t.Fatalf("reading the release: %v", err)
	}
	for _, finalizer := range release.Finalizers {
		if finalizer == addonsv1alpha1.FinalizerRemoval {
			t.Fatalf("a Retain release carries the removal finalizer, so its deletion can block")
		}
	}

	if err := c.Delete(t.Context(), proxy); err != nil {
		t.Fatalf("deleting the proxy: %v", err)
	}
	// Nothing to wait on, so this should be quick rather than eventual - but poll anyway, since
	// "quick" is still asynchronous.
	if err := wait.PollUntilContextTimeout(t.Context(), 2*time.Second, 3*time.Minute, true,
		func(ctx context.Context) (bool, error) {
			err := c.Get(ctx, releaseKey, &addonsv1alpha1.ZarfPackageRelease{})
			return apierrors.IsNotFound(err), client.IgnoreNotFound(err)
		}); err != nil {
		t.Fatalf("the Retain release never went away: %v", err)
	}

	// Orphaned, not removed: the package is still on the cluster with nothing declaring it.
	if err := workload.Get(t.Context(),
		types.NamespacedName{Namespace: "default", Name: appPackageName}, &corev1.ConfigMap{}); err != nil {
		t.Fatalf("Retain removed the package anyway: %v", err)
	}
	if err := workload.Get(t.Context(), deployedPackageRecordName(packageName), &corev1.Secret{}); err != nil {
		t.Fatalf("Retain removed zarf's record anyway: %v", err)
	}

	// Leave the cluster as the other tests expect to find it.
	t.Cleanup(func() {
		ctx := context.Background()
		_ = workload.Delete(ctx, &corev1.ConfigMap{ //nolint:errcheck // best-effort cleanup
			ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: appPackageName},
		})
		_ = workload.Delete(ctx, &corev1.Secret{ //nolint:errcheck // best-effort cleanup
			ObjectMeta: metav1.ObjectMeta{
				Namespace: deployedPackageRecordName(packageName).Namespace,
				Name:      deployedPackageRecordName(packageName).Name,
			},
		})
	})
}
