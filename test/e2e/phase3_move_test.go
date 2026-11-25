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
	"os"
	"os/exec"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	addonsv1alpha1 "github.com/colonel-byte/cluster-api-addon-provider-zarf/api/v1alpha1"
)

// Phase 3 of dist/e2e-test-plan.md, the pivot: `clusterctl move` migrates a management cluster,
// and it is the one operation that handles this provider's objects without going through its
// controller. clusterctl reads them, pauses the Cluster, recreates them on the target and
// deletes the originals - and it only includes a provider's kinds if their CRDs carry
// `clusterctl.cluster.x-k8s.io/move`. Without that label the workload cluster arrives on the new
// management cluster carrying nothing, and the failure is silent on both sides.

// clusterctlPath returns the pinned clusterctl the harness downloaded, or skips.
func clusterctlBin(t *testing.T) string {
	t.Helper()
	path := os.Getenv("E2E_CLUSTERCTL")
	if path == "" {
		t.Skip("E2E_CLUSTERCTL is not set; it is exported by `go run ./magefiles/core e2e:move`")
	}
	return path
}

// TestMoveLabelOnOurCRDs is the cheap guard, and it runs on every suite. `clusterctl move`
// discovers what to carry by listing CRDs that bear `clusterctl.cluster.x-k8s.io/move`, so
// losing that label on either kind is a one-line change in config/crd/kustomization.yaml that
// nothing else notices.
//
// Asserted on the CRDs rather than by reading `clusterctl move --dry-run` output, which reports
// counts by category ("Clusters=1", "ClusterClasses=1") and never names a provider's kinds - so
// grepping it for ours can only ever produce a false failure.
func TestMoveLabelOnOurCRDs(t *testing.T) {
	// clientFor rather than phase1Client: reading a CustomResourceDefinition needs the
	// apiextensions types in the scheme, which the shared client does not carry.
	kubeconfig := os.Getenv("E2E_KUBECONFIG")
	if kubeconfig == "" {
		t.Skip("E2E_KUBECONFIG is not set; run this suite with `go run ./magefiles/core e2e:run`")
	}
	c := clientFor(t, kubeconfig)
	for _, name := range []string{
		"zarfpackageproxies.addons.colonel-byte.dev",
		"zarfpackagereleases.addons.colonel-byte.dev",
	} {
		crd := &apiextensionsv1.CustomResourceDefinition{}
		if err := c.Get(t.Context(), types.NamespacedName{Name: name}, crd); err != nil {
			t.Fatalf("reading CRD %s: %v", name, err)
		}
		if _, ok := crd.Labels["clusterctl.cluster.x-k8s.io/move"]; !ok {
			t.Fatalf("CRD %s does not carry clusterctl.cluster.x-k8s.io/move, so clusterctl "+
				"will leave its objects behind on a management cluster migration", name)
		}
	}
}

// TestClusterctlMove is the real pivot, and it runs only when e2e:move has built a target
// management cluster - it needs a second CAPI, CAPD and CAAPZ install, which roughly doubles the
// topology, so it is not part of the default suite.
//
// What a move actually carries, established by running one rather than assumed:
//
//   - The `Cluster` moves. So does everything in its ownership graph that clusterctl discovers.
//   - A `ZarfPackageProxy` does *not* move. It is deliberately owned by nothing - it selects
//     clusters by label and spans a fleet - so it is in no cluster's object graph, and clusterctl
//     has no reason to carry it. It stays on the old management cluster.
//   - The `ZarfPackageRelease` does not arrive either, and is gone from the source: it is
//     owner-referenced to the `Cluster`, so deleting the source `Cluster` at the end of the move
//     garbage-collects it.
//
// None of which touches the workload cluster. Zarf's own record of what is deployed lives there,
// not here, which is what makes the recovery safe: re-apply the proxies on the new management
// cluster and an ApplyOnce release probes, finds that record, and adopts it rather than
// re-running the seed. That is the guarantee this test exists to hold - the management cluster is
// bookkeeping, and bookkeeping can be rebuilt.
func TestClusterctlMove(t *testing.T) {
	target := os.Getenv("E2E_TARGET_KUBECONFIG")
	if target == "" {
		t.Skip("E2E_TARGET_KUBECONFIG is not set; run this with `go run ./magefiles/core e2e:move`")
	}
	source := phase1Client(t)
	workload := workloadClient(t)
	clusterctl := clusterctlBin(t)
	sourceKubeconfig := os.Getenv("E2E_KUBECONFIG")

	// An ApplyOnce seed is the sharpest subject: re-executing one is precisely what §3
	// promises never happens, and a pivot is where a controller is most tempted to.
	proxy := deployProxy(t, source, "e2e-moved", addonsv1alpha1.PolicyRetain, addonsv1alpha1.StrategyApplyOnce)
	if err := source.Get(t.Context(), client.ObjectKeyFromObject(proxy), proxy); err != nil {
		t.Fatalf("re-reading the proxy: %v", err)
	}
	packageName := proxy.Status.PackageName

	// Mark the deployed content: if anything re-executes on the far side, the package
	// overwrites this.
	cmKey := types.NamespacedName{Namespace: "default", Name: appPackageName}
	configMap := &corev1.ConfigMap{}
	if err := workload.Get(t.Context(), cmKey, configMap); err != nil {
		t.Fatalf("reading the deployed ConfigMap: %v", err)
	}
	beforeEdit := configMap.DeepCopy()
	configMap.Data["marker"] = "survived-the-move"
	if err := workload.Patch(t.Context(), configMap, client.MergeFrom(beforeEdit)); err != nil {
		t.Fatalf("marking the deployed ConfigMap: %v", err)
	}

	out, err := exec.CommandContext(t.Context(), clusterctl, "move",
		"-n", "default", "--kubeconfig", sourceKubeconfig, "--to-kubeconfig", target).CombinedOutput()
	if err != nil {
		t.Fatalf("clusterctl move failed: %v\n%s", err, out)
	}
	t.Logf("clusterctl move:\n%s", out)

	targetClient := clientFor(t, target)

	// The Cluster arrived.
	waitFor(t, "the Cluster never arrived on the target", func(ctx context.Context) (bool, error) {
		err := targetClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: workloadCluster}, &clusterv1.Cluster{})
		return err == nil, client.IgnoreNotFound(err)
	})

	// The proxy did not, and is still where it was. Asserted so the day-2 runbook cannot drift
	// from the behaviour: migrating a management cluster means re-applying the declarations.
	if err := targetClient.Get(t.Context(),
		types.NamespacedName{Namespace: "default", Name: "e2e-moved"},
		&addonsv1alpha1.ZarfPackageProxy{}); !apierrors.IsNotFound(err) {
		t.Fatalf("a proxy arrived on the target unexpectedly (err=%v) - if clusterctl now carries "+
			"fleet-scoped objects, this test and the migration guide both need rewriting", err)
	}
	if err := source.Get(t.Context(), client.ObjectKeyFromObject(proxy), proxy); err != nil {
		t.Fatalf("the proxy left the source as well as not arriving: %v", err)
	}

	// Whatever happened on the management side, the workload cluster is untouched: Zarf's
	// record and the deployed content are both still there.
	if err := workload.Get(t.Context(), deployedPackageRecordName(packageName), &corev1.Secret{}); err != nil {
		t.Fatalf("zarf's record did not survive the move: %v", err)
	}
	if err := workload.Get(t.Context(), cmKey, configMap); err != nil {
		t.Fatalf("re-reading the ConfigMap: %v", err)
	}
	if configMap.Data["marker"] != "survived-the-move" {
		t.Fatalf("the package re-executed during the move: marker is %q", configMap.Data["marker"])
	}

	// Now the recovery, which is the part an operator performs: re-apply the declaration on the
	// new management cluster. The release is rebuilt from nothing, and must complete by
	// adoption - not by deploying the seed a second time.
	rebuilt := &addonsv1alpha1.ZarfPackageProxy{
		ObjectMeta: metav1.ObjectMeta{Name: "e2e-moved", Namespace: "default"},
		Spec:       *proxy.Spec.DeepCopy(),
	}
	if err := targetClient.Create(t.Context(), rebuilt); err != nil {
		t.Fatalf("re-applying the proxy on the target: %v", err)
	}

	releaseKey := types.NamespacedName{Namespace: "default", Name: "e2e-moved-" + workloadCluster}
	waitLong(t, "the re-applied proxy never adopted the existing deployment", func(ctx context.Context) (bool, error) {
		release := &addonsv1alpha1.ZarfPackageRelease{}
		if err := targetClient.Get(ctx, releaseKey, release); err != nil {
			return false, client.IgnoreNotFound(err)
		}
		ready := meta.FindStatusCondition(release.Status.Conditions, addonsv1alpha1.ConditionReady)
		if ready == nil {
			return false, nil
		}
		t.Logf("waiting for adoption on the target: phase=%s reason=%s", release.Status.Phase, ready.Reason)
		if ready.Reason == addonsv1alpha1.ReasonAdoptedExistingRecord {
			return true, nil
		}
		if release.Status.Phase == addonsv1alpha1.PhaseCompleted {
			return false, fmt.Errorf("the rebuilt release completed with reason %q, not by adoption", ready.Reason)
		}
		return false, nil
	})

	// Nothing re-ran: no Job on the target, and the marker is still ours.
	if err := targetClient.Get(t.Context(),
		types.NamespacedName{Namespace: "default", Name: "e2e-moved-" + workloadCluster + "-deploy"},
		&batchv1.Job{}); !apierrors.IsNotFound(err) {
		t.Fatalf("the target stamped a deploy Job for an adopted seed (err=%v)", err)
	}
	if err := workload.Get(t.Context(), cmKey, configMap); err != nil {
		t.Fatalf("re-reading the ConfigMap after adoption: %v", err)
	}
	if configMap.Data["marker"] != "survived-the-move" {
		t.Fatalf("the seed re-executed after the move: marker is %q", configMap.Data["marker"])
	}
}

// clientFor builds a typed client for an arbitrary kubeconfig, which the pivot needs for the
// target management cluster.
func clientFor(t *testing.T, kubeconfig string) client.Client {
	t.Helper()
	cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		t.Fatalf("loading kubeconfig %s: %v", kubeconfig, err)
	}
	scheme := clientgoscheme.Scheme
	if err := clusterv1.AddToScheme(scheme); err != nil {
		t.Fatalf("adding the CAPI scheme: %v", err)
	}
	if err := addonsv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("adding the addons scheme: %v", err)
	}
	// CRDs are read by the move-label test; the client-go scheme does not carry them.
	if err := apiextensionsv1.AddToScheme(scheme); err != nil {
		t.Fatalf("adding the apiextensions scheme: %v", err)
	}
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("building a client for %s: %v", kubeconfig, err)
	}
	return c
}
