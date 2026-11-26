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
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

// The registry-reachability fields (architecture.md "Registry Reachability"), against the
// topology's second registry: HTTPS behind a certificate no public root signs, and basic auth.
// Every assertion here fails on the plain registry, where all five fields are no-ops.
//
// Not covered: fromZarfState, which reads the management cluster's own zarf-state secret. The
// e2e management cluster is a kind cluster that was never zarf-initialized, so there is no
// state to read - covering it means zarf-initializing the management cluster itself, which is
// a topology change rather than a test.
const (
	// Mirrors magefiles/pkg/e2e's secure-registry constants, spelled again for the same
	// reason the fixture constants are: the suite is a separate module, and what it asserts
	// about is the registry, not the code that started it.
	secureRegistryHost     = "caapz-e2e-secure-registry:5000"
	secureRegistryUser     = "caapz"
	secureRegistryPassword = "e2e-registry-password"

	secureAppRepository = secureRegistryHost + "/packages/e2e-app"
)

// secureRegistryCA is the CA e2e:up generated for the secure registry, or a skip when the suite
// is not being driven by e2e:run.
func secureRegistryCA(t *testing.T) []byte {
	t.Helper()
	dir := os.Getenv("E2E_SECURE_REGISTRY_DIR")
	if dir == "" {
		t.Skip("E2E_SECURE_REGISTRY_DIR is not set; run this suite with `go run ./magefiles/core e2e:run`")
	}
	ca, err := os.ReadFile(filepath.Join(dir, "ca.crt"))
	if err != nil {
		t.Fatalf("unable to read the secure registry CA: %v", err)
	}
	return ca
}

// pullSecret creates the dockerconfigjson secret a proxy's pullSecretRef names - the shape
// `kubectl create secret docker-registry` produces, which is the documented way in.
func pullSecret(t *testing.T, c client.Client, name string) {
	t.Helper()
	auth := base64.StdEncoding.EncodeToString([]byte(secureRegistryUser + ":" + secureRegistryPassword))
	config, err := json.Marshal(map[string]any{
		"auths": map[string]any{
			secureRegistryHost: map[string]string{"auth": auth},
		},
	})
	if err != nil {
		t.Fatalf("rendering the docker config: %v", err)
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Type:       corev1.SecretTypeDockerConfigJson,
		Data:       map[string][]byte{corev1.DockerConfigJsonKey: config},
	}
	create(t, c, secret)
}

func caSecret(t *testing.T, c client.Client, name string, ca []byte) {
	t.Helper()
	create(t, c, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Data:       map[string][]byte{"ca.crt": ca},
	})
}

func caConfigMap(t *testing.T, c client.Client, name string, ca []byte) {
	t.Helper()
	create(t, c, &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Data:       map[string]string{"ca.crt": string(ca)},
	})
}

// create makes obj, replacing one a previous run left behind - the suite shares one topology,
// and a stale CA from a recreated registry is worse than no object at all.
func create(t *testing.T, c client.Client, obj client.Object) {
	t.Helper()
	err := c.Create(t.Context(), obj)
	if apierrors.IsAlreadyExists(err) {
		if err := c.Delete(t.Context(), obj); err != nil {
			t.Fatalf("deleting the leftover %T: %v", obj, err)
		}
		obj.SetResourceVersion("")
		err = c.Create(t.Context(), obj)
	}
	if err != nil {
		t.Fatalf("creating the %T: %v", obj, err)
	}
	t.Cleanup(func() {
		_ = c.Delete(context.Background(), obj)
	})
}

// secureProxy is a proxy against the secure registry, selecting nothing, so resolution runs
// without a deploy. Resolution is where credentials and the CA are either enough or not, and a
// proxy matching no cluster reaches it in seconds.
func secureProxy(name string, source addonsv1alpha1.ZarfPackageSource) *addonsv1alpha1.ZarfPackageProxy {
	source.Repository = secureAppRepository
	source.Version = appPackageVersion
	return &addonsv1alpha1.ZarfPackageProxy{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: addonsv1alpha1.ZarfPackageProxySpec{
			// A label nothing carries: releases are stamped per matched cluster, and this test
			// is about the proxy's own resolution.
			ClusterSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{"caapz.e2e/registry-auth": "no-cluster"},
			},
			Package: source,
		},
	}
}

// waitResolution polls the proxy's PackageResolved condition until it settles either way.
func waitResolution(t *testing.T, c client.Client, proxy *addonsv1alpha1.ZarfPackageProxy) *metav1.Condition {
	t.Helper()
	key := client.ObjectKeyFromObject(proxy)
	var settled *metav1.Condition
	if err := wait.PollUntilContextTimeout(t.Context(), 2*time.Second, 3*time.Minute, true,
		func(ctx context.Context) (bool, error) {
			current := &addonsv1alpha1.ZarfPackageProxy{}
			if err := c.Get(ctx, key, current); err != nil {
				return false, err
			}
			condition := meta.FindStatusCondition(current.Status.Conditions, addonsv1alpha1.ConditionPackageResolved)
			if condition == nil {
				return false, nil
			}
			settled = condition
			return true, nil
		}); err != nil {
		t.Fatalf("%s never reported PackageResolved: %v", key.Name, err)
	}
	return settled
}

// TestRegistryAuthResolution is the matrix: what the controller can and cannot reach on the
// secure registry, by which fields the source names. The first case is the one that makes the
// rest mean something - without it, a test suite that only ever names the credentials cannot
// tell a working credential from a registry that never needed one.
func TestRegistryAuthResolution(t *testing.T) {
	c := phase1Client(t)
	ca := secureRegistryCA(t)

	pullSecret(t, c, "secure-registry-creds")
	caSecret(t, c, "secure-registry-ca", ca)
	caConfigMap(t, c, "secure-registry-ca-cm", ca)

	tests := []struct {
		name     string
		source   addonsv1alpha1.ZarfPackageSource
		resolves bool
	}{
		{
			name:   "nothing named",
			source: addonsv1alpha1.ZarfPackageSource{},
		},
		{
			name: "the CA without the credentials",
			source: addonsv1alpha1.ZarfPackageSource{
				CABundleSecretRef: &addonsv1alpha1.NamespacedReference{Name: "secure-registry-ca"},
			},
		},
		{
			name: "the credentials without the CA",
			source: addonsv1alpha1.ZarfPackageSource{
				PullSecretRef: &addonsv1alpha1.NamespacedReference{Name: "secure-registry-creds"},
			},
		},
		{
			name: "the credentials and the CA from secrets",
			source: addonsv1alpha1.ZarfPackageSource{
				PullSecretRef:     &addonsv1alpha1.NamespacedReference{Name: "secure-registry-creds"},
				CABundleSecretRef: &addonsv1alpha1.NamespacedReference{Name: "secure-registry-ca"},
			},
			resolves: true,
		},
		{
			name: "the CA from a configmap",
			source: addonsv1alpha1.ZarfPackageSource{
				PullSecretRef:        &addonsv1alpha1.NamespacedReference{Name: "secure-registry-creds"},
				CABundleConfigMapRef: &addonsv1alpha1.NamespacedReference{Name: "secure-registry-ca-cm"},
			},
			resolves: true,
		},
		{
			name: "insecureSkipTLSVerify instead of the CA",
			source: addonsv1alpha1.ZarfPackageSource{
				PullSecretRef:         &addonsv1alpha1.NamespacedReference{Name: "secure-registry-creds"},
				InsecureSkipTLSVerify: true,
			},
			resolves: true,
		},
		{
			name: "a secret that does not exist",
			source: addonsv1alpha1.ZarfPackageSource{
				PullSecretRef: &addonsv1alpha1.NamespacedReference{Name: "no-such-secret"},
			},
		},
	}

	for i, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			proxy := secureProxy(fmt.Sprintf("secure-resolve-%d", i), test.source)
			deleteProxy(t, c, proxy)
			if err := c.Create(t.Context(), proxy); err != nil {
				t.Fatalf("creating the proxy: %v", err)
			}
			t.Cleanup(func() {
				_ = c.Delete(context.Background(), proxy)
			})

			condition := waitResolution(t, c, proxy)
			resolved := condition.Status == metav1.ConditionTrue
			if resolved != test.resolves {
				t.Fatalf("want resolved=%t, got %s/%s: %s",
					test.resolves, condition.Status, condition.Reason, condition.Message)
			}
			if !resolved {
				return
			}
			current := &addonsv1alpha1.ZarfPackageProxy{}
			if err := c.Get(t.Context(), client.ObjectKeyFromObject(proxy), current); err != nil {
				t.Fatalf("re-reading the proxy: %v", err)
			}
			if !strings.HasPrefix(current.Status.ResolvedDigest, "sha256:") {
				t.Fatalf("want a pinned digest, got %q", current.Status.ResolvedDigest)
			}
			if current.Status.PackageName != appPackageName {
				t.Fatalf("want the %s package, got %q", appPackageName, current.Status.PackageName)
			}
		})
	}
}

// TestRegistryAuthDeploy is the deploy pod's half: the controller resolves with the credentials
// in process, but the pod pulls through a mounted Docker config and a mounted CA
// (docs/agent/choice-registry-credentials.md), and nothing short of a real pull proves that
// mount works. The workload cluster is already zarf-initialized by the Phase 2 test; this test
// deploys an ordinary package, so it needs that state and does not re-establish it.
func TestRegistryAuthDeploy(t *testing.T) {
	c := phase1Client(t)
	workload := workloadClient(t)
	ca := secureRegistryCA(t)

	// The init-first gate holds an ordinary package on a cluster with no zarf state, so this
	// test depends on Phase 2 having initialized it rather than re-doing that work.
	if err := workload.Get(t.Context(),
		types.NamespacedName{Namespace: "zarf", Name: "zarf-state"}, &corev1.Secret{}); err != nil {
		t.Skip("the workload cluster is not zarf-initialized; TestAutoInitAndDeploy establishes that")
	}

	pullSecret(t, c, "secure-registry-creds-deploy")
	caSecret(t, c, "secure-registry-ca-deploy", ca)

	proxy := &addonsv1alpha1.ZarfPackageProxy{
		ObjectMeta: metav1.ObjectMeta{Name: "secure-deploy", Namespace: "default"},
		Spec: addonsv1alpha1.ZarfPackageProxySpec{
			ClusterSelector: metav1.LabelSelector{},
			Package: addonsv1alpha1.ZarfPackageSource{
				Repository:        secureAppRepository,
				Version:           appPackageVersion,
				PullSecretRef:     &addonsv1alpha1.NamespacedReference{Name: "secure-registry-creds-deploy"},
				CABundleSecretRef: &addonsv1alpha1.NamespacedReference{Name: "secure-registry-ca-deploy"},
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

	// The release name the controller stamps per matched cluster, same as Phase 2 asserts on.
	key := types.NamespacedName{Namespace: "default", Name: proxy.Name + "-" + workloadCluster}
	waitLong(t, "the deploy to succeed through the secure registry", func(ctx context.Context) (bool, error) {
		phase, message, err := releasePhase(ctx, c, key)
		if apierrors.IsNotFound(err) {
			// The release is stamped on a later pass than the proxy's creation.
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

	// The ConfigMap the fixture package carries, on the workload cluster: the bytes came
	// through a registry that refused both an anonymous client and an untrusting one.
	deployed := &corev1.ConfigMap{}
	if err := workload.Get(t.Context(),
		types.NamespacedName{Namespace: "default", Name: appPackageName}, deployed); err != nil {
		t.Fatalf("the fixture ConfigMap is not on the workload cluster: %v", err)
	}
	if deployed.Data["marker"] != appPackageName {
		t.Fatalf("want marker %q, got %q", appPackageName, deployed.Data["marker"])
	}
}
