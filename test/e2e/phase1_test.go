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
	"os"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	addonsv1alpha1 "github.com/colonel-byte/cluster-api-addon-provider-zarf/api/v1alpha1"
)

// Phase 1 of dist/e2e-test-plan.md: the controller running in the management cluster, exercised
// against real CAPI objects. Everything here is pure matching/stamping behavior - no package
// exists in the registry yet, so resolution fails on every proxy and that is fine: releases are
// stamped independent of resolution, which is itself worth having pinned on a live cluster.

// phase1Client returns a typed client for the management cluster, or skips when the suite is not
// driven by e2e:run.
func phase1Client(t *testing.T) client.Client {
	t.Helper()
	kubeconfig := os.Getenv("E2E_KUBECONFIG")
	if kubeconfig == "" {
		t.Skip("E2E_KUBECONFIG is not set; run this suite with `go run ./magefiles/core e2e:run`")
	}
	cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		t.Fatalf("unable to load the management kubeconfig: %v", err)
	}
	scheme := clientgoscheme.Scheme
	if err := clusterv1.AddToScheme(scheme); err != nil {
		t.Fatalf("adding the CAPI scheme: %v", err)
	}
	if err := addonsv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("adding the addons scheme: %v", err)
	}
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("unable to build a client: %v", err)
	}
	return c
}

// testProxy returns a Phase 1 proxy fixture: Retain, so deleting it (or un-matching its release)
// never owes a removal Job, and a package reference nothing has staged - resolution failing is
// irrelevant to stamping.
func testProxy(name, namespace string) *addonsv1alpha1.ZarfPackageProxy {
	return &addonsv1alpha1.ZarfPackageProxy{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: addonsv1alpha1.ZarfPackageProxySpec{
			ClusterSelector: metav1.LabelSelector{},
			DeletionPolicy:  addonsv1alpha1.PolicyRetain,
			Package: addonsv1alpha1.ZarfPackageSource{
				Repository: registryHost + "/packages/" + name,
				Version:    "0.0.1",
			},
		},
	}
}

// createProxy creates the fixture and registers best-effort cleanup.
func createProxy(t *testing.T, c client.Client, proxy *addonsv1alpha1.ZarfPackageProxy) {
	t.Helper()
	if err := c.Create(t.Context(), proxy); err != nil {
		t.Fatalf("creating proxy %s: %v", proxy.Name, err)
	}
	t.Cleanup(func() {
		_ = c.Delete(context.Background(), proxy) //nolint:errcheck // best-effort cleanup
	})
}

// waitFor polls until check returns true, failing the test with why on timeout.
func waitFor(t *testing.T, why string, check func(context.Context) (bool, error)) {
	t.Helper()
	if err := wait.PollUntilContextTimeout(t.Context(), 2*time.Second, 2*time.Minute, true, check); err != nil {
		t.Fatalf("%s: %v", why, err)
	}
}

// TestControllerDeployed asserts the e2e:run install itself: the controller deployment is
// Available in caapz-system.
func TestControllerDeployed(t *testing.T) {
	c := phase1Client(t)
	deployment := &appsv1.Deployment{}
	waitFor(t, "the controller deployment never became Available", func(ctx context.Context) (bool, error) {
		if err := c.Get(ctx, types.NamespacedName{Namespace: "caapz-system", Name: "caapz-controller-manager"}, deployment); err != nil {
			return false, err
		}
		for _, cond := range deployment.Status.Conditions {
			if cond.Type == appsv1.DeploymentAvailable && cond.Status == corev1.ConditionTrue {
				return true, nil
			}
		}
		return false, nil
	})
}

// TestReleaseFollowsSelector is Phase 1's core assertion on real CAPI objects: a matching proxy
// stamps a release owned by the Cluster and only the Cluster (§8B's single-owner rule), and an
// un-match deletes it.
func TestReleaseFollowsSelector(t *testing.T) {
	c := phase1Client(t)
	proxy := testProxy("e2e-lifecycle", "default")
	createProxy(t, c, proxy)

	release := &addonsv1alpha1.ZarfPackageRelease{}
	releaseKey := types.NamespacedName{Namespace: "default", Name: "e2e-lifecycle-" + workloadCluster}
	waitFor(t, "the release was never stamped for the matching cluster", func(ctx context.Context) (bool, error) {
		err := c.Get(ctx, releaseKey, release)
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return err == nil, err
	})

	if len(release.OwnerReferences) != 1 {
		t.Fatalf("release owners = %+v, want exactly the Cluster", release.OwnerReferences)
	}
	owner := release.OwnerReferences[0]
	if owner.Kind != "Cluster" || owner.Name != workloadCluster || owner.Controller == nil || !*owner.Controller {
		t.Fatalf("release owner = %+v, want the controlling Cluster %s", owner, workloadCluster)
	}
	if release.Spec.ClusterRef.Name != workloadCluster {
		t.Fatalf("release clusterRef = %q, want %s", release.Spec.ClusterRef.Name, workloadCluster)
	}

	// Un-match: the release goes away without the Cluster being touched.
	before := proxy.DeepCopy()
	proxy.Spec.ClusterSelector = metav1.LabelSelector{MatchLabels: map[string]string{"e2e": "never-matches"}}
	if err := c.Patch(t.Context(), proxy, client.MergeFrom(before)); err != nil {
		t.Fatalf("patching the selector: %v", err)
	}
	waitFor(t, "the release survived the selector un-match", func(ctx context.Context) (bool, error) {
		err := c.Get(ctx, releaseKey, &addonsv1alpha1.ZarfPackageRelease{})
		return apierrors.IsNotFound(err), client.IgnoreNotFound(err)
	})
}

// TestForeignNamespaceOutOfScope pins §2C's default scope on a live cluster: without
// --cluster-namespaces, a proxy in another namespace never reaches the Cluster in default - it
// reconciles to zero matches and stamps nothing anywhere.
func TestForeignNamespaceOutOfScope(t *testing.T) {
	c := phase1Client(t)
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "caapz-e2e-foreign"}}
	if err := c.Create(t.Context(), ns); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("creating the foreign namespace: %v", err)
	}
	t.Cleanup(func() {
		_ = c.Delete(context.Background(), ns) //nolint:errcheck // best-effort cleanup
	})
	proxy := testProxy("e2e-foreign", "caapz-e2e-foreign")
	createProxy(t, c, proxy)

	// Proof of "reconciled and matched nothing", not just "nothing happened yet": the status
	// records the generation with an empty match list.
	waitFor(t, "the foreign proxy was never reconciled", func(ctx context.Context) (bool, error) {
		if err := c.Get(ctx, client.ObjectKeyFromObject(proxy), proxy); err != nil {
			return false, err
		}
		return proxy.Status.ObservedGeneration == proxy.Generation, nil
	})
	if len(proxy.Status.MatchingClusters) != 0 {
		t.Fatalf("matchingClusters = %v, want none from a foreign namespace", proxy.Status.MatchingClusters)
	}
	err := c.Get(t.Context(), types.NamespacedName{Namespace: "caapz-e2e-foreign", Name: "e2e-foreign-" + workloadCluster}, &addonsv1alpha1.ZarfPackageRelease{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("a release exists for a foreign-namespace proxy (err=%v)", err)
	}
}

// TestDependsOnVerdicts observes §5B's three hold verdicts as conditions on live releases: a
// dependency that matches but is not deployed, one whose selector never matches this cluster,
// and one that names no proxy at all.
func TestDependsOnVerdicts(t *testing.T) {
	c := phase1Client(t)

	base := testProxy("e2e-dep-base", "default")
	createProxy(t, c, base)
	narrow := testProxy("e2e-dep-narrow", "default")
	narrow.Spec.ClusterSelector = metav1.LabelSelector{MatchLabels: map[string]string{"e2e": "never-matches"}}
	createProxy(t, c, narrow)

	cases := []struct {
		name      string
		dependsOn string
		want      string
	}{
		{
			name:      "e2e-dep-waiting",
			dependsOn: "e2e-dep-base",
			want:      addonsv1alpha1.ReasonWaitingForDependency,
		},
		{
			name:      "e2e-dep-inapplicable",
			dependsOn: "e2e-dep-narrow",
			want:      addonsv1alpha1.ReasonDependencyNotApplicable,
		},
		{
			name:      "e2e-dep-missing",
			dependsOn: "e2e-dep-ghost",
			want:      addonsv1alpha1.ReasonDependencyNotFound,
		},
	}
	for _, tc := range cases {
		proxy := testProxy(tc.name, "default")
		proxy.Spec.DependsOn = []addonsv1alpha1.DependencyReference{{Name: tc.dependsOn}}
		createProxy(t, c, proxy)
	}
	for _, tc := range cases {
		release := &addonsv1alpha1.ZarfPackageRelease{}
		releaseKey := types.NamespacedName{Namespace: "default", Name: tc.name + "-" + workloadCluster}
		waitFor(t, tc.name+" never carried the expected verdict", func(ctx context.Context) (bool, error) {
			if err := c.Get(ctx, releaseKey, release); err != nil {
				return false, client.IgnoreNotFound(err)
			}
			cond := meta.FindStatusCondition(release.Status.Conditions, addonsv1alpha1.ConditionDependenciesSatisfied)
			return cond != nil && cond.Reason == tc.want, nil
		})
	}
}
