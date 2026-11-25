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
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	addonsv1alpha1 "github.com/colonel-byte/cluster-api-addon-provider-zarf/api/v1alpha1"
)

// Phase 3 of dist/e2e-test-plan.md, lifecycle half: §8C's override, that destroying a workload
// cluster is never blocked by a package deployed to it. This is the one assertion in the suite
// that has to destroy a cluster, so it provisions its own rather than spending the standing one
// the rest of the suite is built around.

const disposableCluster = "e2e-disposable"

// disposableRoleLabel marks the throwaway cluster so this test's proxy can select it alone. The
// other proxies in the suite select every cluster, which is fine - a release stamped on a
// cluster with no Zarf state just holds - but this test's proxy must not reach the standing one,
// because it is about to delete what it matches.
const disposableRoleLabel = "e2e.addons.colonel-byte.dev/role"

// suiteOwnedLabel marks infrastructure this suite created, and is the standing rule for anything
// in here that destroys a cluster: the suite deletes only what it made. The standing topology
// belongs to `e2e:up`, carries no such label, and is therefore undeletable from a test - which
// is the guard rail, because the expensive mistakes in this file are all the same shape. A test
// aimed at one object reaches another, and the damage is done before anything reports it.
//
// Enforced by requireSuiteOwned below rather than by remembering.
const suiteOwnedLabel = "e2e.addons.colonel-byte.dev/owned-by"

// suiteOwner is the value suiteOwnedLabel carries.
const suiteOwner = "caapz-e2e-suite"

// requireSuiteOwned fails the test unless obj carries the suite's ownership label. Call it
// before deleting anything that costs minutes to rebuild or that other tests depend on.
func requireSuiteOwned(t *testing.T, obj client.Object) {
	t.Helper()
	if obj.GetLabels()[suiteOwnedLabel] != suiteOwner {
		t.Fatalf("refusing to delete %s/%s: it does not carry %s=%s, so this suite did not create "+
			"it - deleting it would destroy something the topology or another test owns",
			obj.GetNamespace(), obj.GetName(), suiteOwnedLabel, suiteOwner)
	}
}

// provisionDisposableCluster creates a second CAPD cluster by copying the standing cluster's
// *topology*, which is how it avoids hand-maintaining one: the ClusterClass the topology names
// is already installed, and carrying the topology over means every variable that class requires
// comes along without this test having to know what they are. Returns once CAPI reports the
// control plane initialized, which is what makes the controller stamp releases for it.
//
// Only the topology and the network, never the whole spec. A Cluster's spec also holds
// `controlPlaneRef` and `infrastructureRef`, which the topology controller fills in with the
// objects it created - so copying the spec wholesale points the new Cluster at the standing
// cluster's control plane and infrastructure. Nothing complains: the new cluster simply never
// gets machines of its own, and deleting it takes the standing cluster's control plane with it,
// because deleting a Cluster deletes whatever those two refs name.
func provisionDisposableCluster(t *testing.T, c client.Client) *clusterv1.Cluster {
	t.Helper()

	standing := &clusterv1.Cluster{}
	if err := c.Get(t.Context(),
		types.NamespacedName{Namespace: "default", Name: workloadCluster}, standing); err != nil {
		t.Fatalf("reading the standing cluster to copy its topology: %v", err)
	}

	cluster := &clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      disposableCluster,
			Namespace: "default",
			Labels: map[string]string{
				disposableRoleLabel: "disposable",
				suiteOwnedLabel:     suiteOwner,
			},
		},
		Spec: clusterv1.ClusterSpec{
			ClusterNetwork: *standing.Spec.ClusterNetwork.DeepCopy(),
			Topology:       *standing.Spec.Topology.DeepCopy(),
		},
	}
	// A previous interrupted run may have left one mid-deletion; wait it out rather than
	// colliding with it.
	deleteDisposableCluster(t, c)
	if err := c.Create(t.Context(), cluster); err != nil {
		t.Fatalf("creating the disposable cluster: %v", err)
	}

	waitLong(t, "the disposable cluster never initialized its control plane", func(ctx context.Context) (bool, error) {
		fresh := &clusterv1.Cluster{}
		if err := c.Get(ctx, client.ObjectKeyFromObject(cluster), fresh); err != nil {
			return false, err
		}
		initialized := fresh.Status.Initialization.ControlPlaneInitialized
		t.Logf("waiting for the disposable cluster: phase=%s initialized=%v", fresh.Status.Phase, initialized != nil && *initialized)
		return initialized != nil && *initialized, nil
	})
	return cluster
}

// deleteDisposableCluster removes the throwaway cluster and waits for it to go, tolerating one
// that was never there. Generous but bounded: CAPD has containers to tear down, and the point of
// the test is that nothing of ours holds this up, so an unbounded wait would hide a block.
func deleteDisposableCluster(t *testing.T, c client.Client) {
	t.Helper()
	cluster := &clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: disposableCluster}}

	// Deleting a Cluster deletes whatever its controlPlaneRef and infrastructureRef name, so
	// refuse if either points at something belonging to another cluster. This is a guard
	// against one specific mistake that has already been made once: building this cluster from
	// a copy of the standing cluster's whole spec, refs included, which turns this delete into
	// a delete of the standing cluster's control plane. Bail rather than destroy the topology.
	existing := &clusterv1.Cluster{}
	err := c.Get(t.Context(), client.ObjectKeyFromObject(cluster), existing)
	switch {
	case apierrors.IsNotFound(err):
		return
	case err != nil:
		t.Fatalf("reading the disposable cluster before deleting it: %v", err)
	}
	requireSuiteOwned(t, existing)
	for field, name := range map[string]string{
		"controlPlaneRef":   existing.Spec.ControlPlaneRef.Name,
		"infrastructureRef": existing.Spec.InfrastructureRef.Name,
	} {
		if name != "" && !strings.HasPrefix(name, disposableCluster) {
			t.Fatalf("refusing to delete %s: its %s names %q, which belongs to another cluster - "+
				"deleting it would take that cluster's objects too",
				disposableCluster, field, name)
		}
	}

	if err := c.Delete(t.Context(), cluster); err != nil && !apierrors.IsNotFound(err) {
		t.Fatalf("deleting the disposable cluster: %v", err)
	}
	if err := wait.PollUntilContextTimeout(t.Context(), 5*time.Second, 10*time.Minute, true,
		func(ctx context.Context) (bool, error) {
			err := c.Get(ctx, client.ObjectKeyFromObject(cluster), &clusterv1.Cluster{})
			return apierrors.IsNotFound(err), nil
		}); err != nil {
		t.Fatalf("the disposable cluster never finished deleting: %v", err)
	}
}

// TestClusterDeletionIsNotBlocked is §8C: a Delete-policy release exists for a cluster, the
// cluster is destroyed, and the destruction completes. §7C is what makes it work - a deleting or
// gone Cluster is infrastructure teardown, so the controller drops the removal finalizer without
// attempting an uninstall, because there is nothing left to uninstall toward and trying would
// block the teardown on a cluster that cannot answer.
//
// Deliberately no package is deployed to the disposable cluster first. The path under test skips
// removal entirely, so a deployed package would never be touched by it, and provisioning a
// second cluster all the way through init to find that out would cost minutes for no extra
// signal. What the test does need - and asserts - is that the release really did carry the
// removal finalizer, so dropping it is a decision rather than a no-op.
func TestClusterDeletionIsNotBlocked(t *testing.T) {
	c := phase1Client(t)

	cluster := provisionDisposableCluster(t, c)
	t.Cleanup(func() {
		_ = c.Delete(context.Background(), cluster) //nolint:errcheck // best-effort cleanup
	})

	proxy := &addonsv1alpha1.ZarfPackageProxy{
		ObjectMeta: metav1.ObjectMeta{Name: "e2e-doomed", Namespace: "default"},
		Spec: addonsv1alpha1.ZarfPackageProxySpec{
			ClusterSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{disposableRoleLabel: "disposable"},
			},
			DeletionPolicy: addonsv1alpha1.PolicyDelete,
			Package: addonsv1alpha1.ZarfPackageSource{
				Repository: appPackageRepository,
				Version:    appPackageVersion,
				API:        addonsv1alpha1.PackageAPIV1Beta1,
			},
		},
	}
	deleteProxy(t, c, proxy)
	if err := c.Create(t.Context(), proxy); err != nil {
		t.Fatalf("creating the proxy: %v", err)
	}
	t.Cleanup(func() {
		_ = c.Delete(context.Background(), proxy) //nolint:errcheck // best-effort cleanup
	})

	// The release has to reach the state the override is about: owed a removal, and carrying
	// the finalizer that owes it.
	releaseKey := types.NamespacedName{Namespace: "default", Name: "e2e-doomed-" + disposableCluster}
	release := &addonsv1alpha1.ZarfPackageRelease{}
	waitLong(t, "the release never carried the removal finalizer", func(ctx context.Context) (bool, error) {
		if err := c.Get(ctx, releaseKey, release); err != nil {
			return false, client.IgnoreNotFound(err)
		}
		for _, finalizer := range release.Finalizers {
			if finalizer == addonsv1alpha1.FinalizerRemoval {
				return true, nil
			}
		}
		t.Logf("waiting for the removal finalizer: finalizers=%v", release.Finalizers)
		return false, nil
	})

	// Nothing of ours may sit on the Cluster itself - §7's rule. A finalizer here would block
	// deletion no matter what the release does.
	if err := c.Get(t.Context(), client.ObjectKeyFromObject(cluster), cluster); err != nil {
		t.Fatalf("re-reading the cluster: %v", err)
	}
	for _, finalizer := range cluster.Finalizers {
		if strings.Contains(finalizer, "colonel-byte") {
			t.Fatalf("the controller put finalizer %q on the Cluster, which blocks its deletion", finalizer)
		}
	}

	// Destroy it. This is the assertion: it completes.
	deleteDisposableCluster(t, c)

	// The release goes with it, and no removal Job was ever stamped - removal toward a cluster
	// that is being torn down is skipped, not attempted and failed.
	if err := wait.PollUntilContextTimeout(t.Context(), 2*time.Second, 5*time.Minute, true,
		func(ctx context.Context) (bool, error) {
			err := c.Get(ctx, releaseKey, &addonsv1alpha1.ZarfPackageRelease{})
			return apierrors.IsNotFound(err), nil
		}); err != nil {
		t.Fatalf("the release outlived its cluster: %v", err)
	}
	err := c.Get(t.Context(),
		types.NamespacedName{Namespace: "default", Name: "e2e-doomed-" + disposableCluster + "-remove"},
		&batchv1.Job{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("a removal Job was stamped toward a deleting cluster (err=%v)", err)
	}
}
