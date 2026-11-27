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

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	zarfconfig "github.com/zarf-dev/zarf/src/config"
	zarfstate "github.com/zarf-dev/zarf/src/pkg/state"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// The controller's two workload-cluster reads, both single-secret probes of observed state
// (docs/dev/architecture.md §3, §6 step 5): has zarf initialized this cluster, and does this
// cluster carry a package's deployed-package record. Deploys themselves never run through these
// credentials - that is the deploy Job's own mount.

// kubeconfigSecretKey is the key CAPI writes the admin kubeconfig under in the
// <cluster-name>-kubeconfig secret.
const kubeconfigSecretKey = "value"

// workloadClient builds a client for clusterName's workload cluster from the kubeconfig secret
// CAPI maintains beside it. A fresh client per call, no cache: probes are rare single reads, and
// a cached connection to a cluster mid-teardown is worse than a dial.
func (r *ZarfPackageProxyReconciler) workloadClient(ctx context.Context, namespace, clusterName string) (client.Client, error) {
	secret := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: clusterName + "-kubeconfig"}, secret); err != nil {
		return nil, fmt.Errorf("reading the %s-kubeconfig secret: %w", clusterName, err)
	}
	raw, ok := secret.Data[kubeconfigSecretKey]
	if !ok {
		return nil, fmt.Errorf("the %s-kubeconfig secret has no %q key", clusterName, kubeconfigSecretKey)
	}
	cfg, err := clientcmd.RESTConfigFromKubeConfig(raw)
	if err != nil {
		return nil, fmt.Errorf("parsing the %s kubeconfig: %w", clusterName, err)
	}
	// An unreachable cluster degrades per §7B; it must not hang a reconcile worker for the
	// client-go default of no timeout at all.
	cfg.Timeout = 10 * time.Second
	return client.New(cfg, client.Options{})
}

// hasZarfState reports whether the cluster behind c has been zarf-initialized - §6 step 5's
// init-first eligibility gate reads exactly this, never a package or proxy name.
func hasZarfState(ctx context.Context, c client.Client) (bool, error) {
	return hasZarfSecret(ctx, c, zarfstate.ZarfStateSecretName)
}

// clusterRegistryMode reads the registry mode the cluster's own zarf-state records - the
// ground truth the init-settings guard compares a declaration against. Empty, never an error,
// when the cluster has no state, the secret is malformed, or the state predates modes: an
// unreadable mode must degrade to "unknown, do not hold" rather than fail the pass (§7B).
func clusterRegistryMode(ctx context.Context, c client.Client) (string, error) {
	secret := &corev1.Secret{}
	err := c.Get(ctx, types.NamespacedName{Namespace: zarfstate.ZarfNamespaceName, Name: zarfstate.ZarfStateSecretName}, secret)
	if apierrors.IsNotFound(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	s := &zarfstate.State{}
	if err := json.Unmarshal(secret.Data[zarfstate.ZarfStateDataKey], s); err != nil {
		return "", nil
	}
	return string(s.RegistryInfo.RegistryMode), nil
}

// hasDeployedPackage reports whether the cluster carries zarf's deployed-package record for the
// package named name - the durable half of §3's two-tier seed check. Adoption is keyed on the
// zarf package name alone, by design.
func hasDeployedPackage(ctx context.Context, c client.Client, name string) (bool, error) {
	return hasZarfSecret(ctx, c, zarfconfig.ZarfPackagePrefix+name)
}

func hasZarfSecret(ctx context.Context, c client.Client, name string) (bool, error) {
	err := c.Get(ctx, types.NamespacedName{Namespace: zarfstate.ZarfNamespaceName, Name: name}, &corev1.Secret{})
	switch {
	case err == nil:
		return true, nil
	case apierrors.IsNotFound(err):
		return false, nil
	}
	return false, err
}
