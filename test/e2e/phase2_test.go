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
	"path/filepath"
	"strings"
	"testing"
	"time"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	addonsv1alpha1 "github.com/colonel-byte/cluster-api-addon-provider-zarf/api/v1alpha1"
)

// Phase 2 of dist/e2e-test-plan.md: a package actually deployed to the workload cluster. The
// happy path runs through auto-init first, because a normal package cannot deploy to a cluster
// with no Zarf state - the init-first eligibility gate holds it (architecture.md §6 step 5) -
// and auto-init is how the gate is satisfied from a declaration rather than by hand.

const (
	// appPackageRepository and friends mirror magefiles/pkg/e2e's fixture constants. Spelled
	// again rather than shared: the suite is a separate module, and what it asserts about is
	// the registry's contents, not the code that put them there.
	appPackageRepository = registryHost + "/packages/e2e-app"
	appPackageVersion    = "0.1.0"
	appPackageName       = "e2e-app"
	// The init package is init-slim, which is zarf.dev/v1beta1 and therefore carries the
	// component service names zarf's init orchestration switches on. Upstream's own published
	// init packages are v1alpha1, which deploy through the SDK without starting anything.
	initPackageRepository = registryHost + "/packages/init-slim"
	initPackageVersion    = "v1.0.1"
	initPackageName       = "init-slim"
	signingKeySecretName  = "e2e-cosign-key"
)

// workloadClient returns a client for the workload cluster - what a deploy's results are read
// through - or skips when the suite is not being driven by e2e:run.
func workloadClient(t *testing.T) client.Client {
	t.Helper()
	kubeconfig := os.Getenv("E2E_WORKLOAD_KUBECONFIG")
	if kubeconfig == "" {
		t.Skip("E2E_WORKLOAD_KUBECONFIG is not set; run this suite with `go run ./magefiles/core e2e:run`")
	}
	cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		t.Fatalf("unable to load the workload kubeconfig: %v", err)
	}
	c, err := client.New(cfg, client.Options{})
	if err != nil {
		t.Fatalf("unable to build a workload client: %v", err)
	}
	return c
}

// publicKeySecret puts one of the fixture public keys - "cosign" for the pair the packages were
// signed with, "wrong" for the unrelated one - in the proxy's namespace under name. Naming such
// a secret on a proxy makes signature verification a precondition of deploying (the Signature
// Trust Model).
func publicKeySecret(t *testing.T, c client.Client, name, pair string) {
	t.Helper()
	dir := os.Getenv("E2E_KEY_DIR")
	if dir == "" {
		t.Skip("E2E_KEY_DIR is not set; run this suite with `go run ./magefiles/core e2e:run`")
	}
	key, err := os.ReadFile(filepath.Join(dir, pair+".pub"))
	if err != nil {
		t.Fatalf("unable to read the %s fixture public key: %v", pair, err)
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Data:       map[string][]byte{"cosign.pub": key},
	}
	if err := c.Create(t.Context(), secret); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("creating the %s key secret: %v", pair, err)
	}
}

// deleteProxy removes a proxy left over from an earlier run and waits for it to go. Proxies
// carry a cleanup finalizer, so the object outlives the delete call by however long its releases
// take to finalize - creating the replacement before that lands fails with "already exists".
func deleteProxy(t *testing.T, c client.Client, proxy *addonsv1alpha1.ZarfPackageProxy) {
	t.Helper()
	if err := c.Delete(t.Context(), proxy); err != nil {
		if apierrors.IsNotFound(err) {
			return
		}
		t.Fatalf("deleting the leftover proxy: %v", err)
	}
	if err := wait.PollUntilContextTimeout(t.Context(), 2*time.Second, 5*time.Minute, true,
		func(ctx context.Context) (bool, error) {
			err := c.Get(ctx, client.ObjectKeyFromObject(proxy), &addonsv1alpha1.ZarfPackageProxy{})
			return apierrors.IsNotFound(err), nil
		}); err != nil {
		t.Fatalf("the leftover proxy never finalized: %v", err)
	}
	// Delete leaves the object's resourceVersion and UID set, which Create rejects.
	proxy.ObjectMeta.ResourceVersion = ""
	proxy.ObjectMeta.UID = ""
}

// resetZarfState de-initializes the workload cluster, which is this test's precondition: the
// auto-init path is keyed on observed cluster state, so on a cluster that already carries
// zarf-state the controller correctly does nothing and there is no init to observe. The suite
// shares one long-lived topology, so that state is whatever the last run left behind.
//
// The agent's MutatingWebhookConfiguration is cluster-scoped and goes first: left behind while
// its service disappears with the namespace, it fails admission for every pod created
// afterwards - including the ones the next init tries to start.
func resetZarfState(t *testing.T, workload client.Client) {
	t.Helper()
	hook := &admissionv1.MutatingWebhookConfiguration{ObjectMeta: metav1.ObjectMeta{Name: "zarf"}}
	if err := workload.Delete(t.Context(), hook); err != nil && !apierrors.IsNotFound(err) {
		t.Fatalf("deleting the zarf agent webhook: %v", err)
	}
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "zarf"}}
	if err := workload.Delete(t.Context(), namespace); err != nil && !apierrors.IsNotFound(err) {
		t.Fatalf("deleting the zarf namespace: %v", err)
	}
	if err := wait.PollUntilContextTimeout(t.Context(), 2*time.Second, 5*time.Minute, true,
		func(ctx context.Context) (bool, error) {
			err := workload.Get(ctx, types.NamespacedName{Name: "zarf"}, &corev1.Namespace{})
			return apierrors.IsNotFound(err), nil
		}); err != nil {
		t.Fatalf("the zarf namespace never finished terminating: %v", err)
	}
}

// waitLong polls for the duration a real deploy needs - the init package pulls hundreds of
// megabytes into the deploy pod and brings up a registry, an injector, and the agent.
func waitLong(t *testing.T, why string, check func(context.Context) (bool, error)) {
	t.Helper()
	if err := wait.PollUntilContextTimeout(t.Context(), 5*time.Second, 20*time.Minute, true, check); err != nil {
		t.Fatalf("%s: %v", why, err)
	}
}

// releasePhase reads one release's phase and Ready reason, for progress logging and assertions.
func releasePhase(ctx context.Context, c client.Client, key types.NamespacedName) (string, string, error) {
	release := &addonsv1alpha1.ZarfPackageRelease{}
	if err := c.Get(ctx, key, release); err != nil {
		return "", "", err
	}
	reason := ""
	if ready := meta.FindStatusCondition(release.Status.Conditions, addonsv1alpha1.ConditionReady); ready != nil {
		reason = ready.Reason
	}
	return string(release.Status.Phase), reason, nil
}

// TestAutoInitAndDeploy is Phase 2's happy path, end to end on real clusters: an autoInit proxy
// initializes the workload cluster through an init Job, then its own signed, digest-pinned
// package deploys through a deploy Job, and the ConfigMap the package carries exists on the
// workload cluster.
//
// One test rather than two, because the init it performs is the precondition of the deploy and
// re-running it per test would cost twenty minutes each time.
func TestAutoInitAndDeploy(t *testing.T) {
	c := phase1Client(t)
	workload := workloadClient(t)
	publicKeySecret(t, c, signingKeySecretName, "cosign")
	resetZarfState(t, workload)

	proxy := &addonsv1alpha1.ZarfPackageProxy{
		ObjectMeta: metav1.ObjectMeta{Name: "e2e-app", Namespace: "default"},
		Spec: addonsv1alpha1.ZarfPackageProxySpec{
			ClusterSelector: metav1.LabelSelector{},
			DeletionPolicy:  addonsv1alpha1.PolicyRetain,
			Package: &addonsv1alpha1.ZarfPackageSource{
				Repository:            appPackageRepository,
				Version:               appPackageVersion,
				API:                   addonsv1alpha1.PackageAPIV1Beta1,
				SignatureKeySecretRef: &addonsv1alpha1.NamespacedReference{Name: signingKeySecretName},
			},
			Init: &addonsv1alpha1.InitOptions{
				AutoInit: true,
				Package: &addonsv1alpha1.ZarfPackageSource{
					Repository: initPackageRepository,
					Version:    initPackageVersion,
					API:        addonsv1alpha1.PackageAPIV1Beta1,
				},
				// init-slim marks every component optional so a cluster can be brought up in
				// stages, which means the default selection is empty and naming them is not
				// optional for us. git-server is left out: nothing here needs it.
				Components: []addonsv1alpha1.ComponentSelector{
					{
						Name: "zarf-injector",
					},
					{
						Name: "zarf-seed-registry",
					},
					{
						Name: "zarf-registry",
					},
					{
						Name: "zarf-agent",
					},
				},
				// init-slim ships `seedRegistry.persistence.enabled: false`, and the registry
				// chart upstream publishes has no storage-backend value to fall back on - its
				// schema is closed - so the seed registry starts with no storage configuration
				// at all and crashloops, which stops the init at its first component. Turning
				// persistence back on is the only lever from this side; the topology gives the
				// workload cluster a default StorageClass for the volume to bind to. Drop this
				// once init-slim ships a seed registry that can start.
				Values: &apiextensionsv1.JSON{
					Raw: []byte(`{"seedRegistry":{"persistence":{"enabled":true}}}`),
				},
			},
		},
	}
	// A run that was interrupted mid-deploy leaves its proxy behind, and the whole suite shares
	// one topology - so start from a clean declaration rather than depending on the last run
	// having finished its cleanup.
	deleteProxy(t, c, proxy)
	if err := c.Create(t.Context(), proxy); err != nil {
		t.Fatalf("creating the proxy: %v", err)
	}
	t.Cleanup(func() {
		_ = c.Delete(context.Background(), proxy) //nolint:errcheck // best-effort cleanup
	})

	// Both packages resolve and verify in the controller, before any Job exists (§6 step 4).
	waitLong(t, "the proxy never resolved both packages", func(ctx context.Context) (bool, error) {
		if err := c.Get(ctx, client.ObjectKeyFromObject(proxy), proxy); err != nil {
			return false, err
		}
		return proxy.Status.ResolvedDigest != "" && proxy.Status.InitResolvedDigest != "", nil
	})
	if !strings.HasPrefix(proxy.Status.ResolvedDigest, "sha256:") {
		t.Fatalf("resolvedDigest = %q, want a digest", proxy.Status.ResolvedDigest)
	}
	if proxy.Status.PackageKind != "ZarfPackageConfig" {
		t.Fatalf("packageKind = %q, want ZarfPackageConfig", proxy.Status.PackageKind)
	}
	verified := meta.FindStatusCondition(proxy.Status.Conditions, addonsv1alpha1.ConditionPackageSignatureVerified)
	if verified == nil || verified.Status != metav1.ConditionTrue {
		t.Fatalf("PackageSignatureVerified = %+v, want True - the fixture is signed and the key is named", verified)
	}
	initResolved := meta.FindStatusCondition(proxy.Status.Conditions, addonsv1alpha1.ConditionInitPackageResolved)
	if initResolved == nil || initResolved.Status != metav1.ConditionTrue {
		t.Fatalf("InitPackageResolved = %+v, want True", initResolved)
	}

	// The init Job runs first and creates Zarf state on the workload cluster. That state, not
	// any status field, is the record auto-init keys on (§6 step 5).
	releaseKey := types.NamespacedName{Namespace: "default", Name: "e2e-app-" + workloadCluster}
	waitLong(t, "the workload cluster never gained zarf state", func(ctx context.Context) (bool, error) {
		phase, reason, err := releasePhase(ctx, c, releaseKey)
		if err != nil {
			return false, client.IgnoreNotFound(err)
		}
		if reason == addonsv1alpha1.ReasonInitFailed {
			return false, terminalFailure(ctx, c, releaseKey, "the init Job failed")
		}
		t.Logf("waiting for zarf state: phase=%s reason=%s", phase, reason)
		err = workload.Get(ctx, types.NamespacedName{Namespace: "zarf", Name: "zarf-state"}, &corev1.Secret{})
		return err == nil, client.IgnoreNotFound(err)
	})

	// zarf state alone is not an initialized cluster: zarf writes state early, and an init that
	// fails afterwards - or one whose components carry no service metadata to orchestrate from -
	// leaves state behind with nothing running. So the assertion is the workloads themselves.
	// The agent is what makes this sound rather than approximate: it is the last component the
	// init package deploys, so an agent pod running means everything before it succeeded.
	waitLong(t, "the init package left no running workloads in the zarf namespace",
		func(ctx context.Context) (bool, error) {
			pods := &corev1.PodList{}
			if err := workload.List(ctx, pods, client.InNamespace("zarf")); err != nil {
				return false, err
			}
			running := map[string]bool{}
			for i := range pods.Items {
				if pods.Items[i].Status.Phase == corev1.PodRunning {
					// Pod names carry their workload's name, which is enough to tell the
					// registry and the agent apart without reading owner chains.
					if strings.Contains(pods.Items[i].Name, "registry") {
						running["registry"] = true
					}
					if strings.Contains(pods.Items[i].Name, "agent") {
						running["agent"] = true
					}
				}
			}
			t.Logf("waiting for the zarf workloads: %d pods, running=%v", len(pods.Items), running)
			return running["registry"] && running["agent"], nil
		})

	// With state present the gate opens and the package's own deploy Job runs.
	waitLong(t, "the release never reached Deployed", func(ctx context.Context) (bool, error) {
		phase, reason, err := releasePhase(ctx, c, releaseKey)
		if err != nil {
			return false, client.IgnoreNotFound(err)
		}
		if reason == addonsv1alpha1.ReasonDeployFailed {
			return false, terminalFailure(ctx, c, releaseKey, "the deploy Job failed")
		}
		t.Logf("waiting for the deploy: phase=%s reason=%s", phase, reason)
		return phase == string(addonsv1alpha1.PhaseDeployed), nil
	})

	release := &addonsv1alpha1.ZarfPackageRelease{}
	if err := c.Get(t.Context(), releaseKey, release); err != nil {
		t.Fatalf("reading the release: %v", err)
	}
	if release.Status.DeployedDigest != proxy.Status.ResolvedDigest {
		t.Fatalf("deployedDigest = %q, want the resolved %q",
			release.Status.DeployedDigest, proxy.Status.ResolvedDigest)
	}
	if release.Status.DeployedVersion != appPackageVersion {
		t.Fatalf("deployedVersion = %q, want %s", release.Status.DeployedVersion, appPackageVersion)
	}

	// The point of all of it: the package's content is on the workload cluster.
	configMap := &corev1.ConfigMap{}
	if err := workload.Get(t.Context(),
		types.NamespacedName{Namespace: "default", Name: appPackageName}, configMap); err != nil {
		t.Fatalf("the package's ConfigMap is not on the workload cluster: %v", err)
	}
	if configMap.Data["marker"] != appPackageName {
		t.Fatalf("ConfigMap marker = %q, want %s", configMap.Data["marker"], appPackageName)
	}

	// Zarf's own record of the deploy, which §3's adoption and removal both key on.
	if err := workload.Get(t.Context(),
		types.NamespacedName{Namespace: "zarf", Name: "zarf-package-" + appPackageName},
		&corev1.Secret{}); err != nil {
		t.Fatalf("zarf left no deployed-package record for %s: %v", appPackageName, err)
	}
}

// terminalFailure ends a poll on a failure condition, carrying the message the pod reported -
// otherwise a failed Job reads as a bare timeout many minutes later with nothing to go on.
func terminalFailure(ctx context.Context, c client.Client, key types.NamespacedName, what string) error {
	release := &addonsv1alpha1.ZarfPackageRelease{}
	if err := c.Get(ctx, key, release); err != nil {
		return err
	}
	message := ""
	if ready := meta.FindStatusCondition(release.Status.Conditions, addonsv1alpha1.ConditionReady); ready != nil {
		message = ready.Message
	}
	return fmt.Errorf("%s: %s", what, message)
}
