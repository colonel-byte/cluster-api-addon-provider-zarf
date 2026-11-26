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
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	zarfstate "github.com/zarf-dev/zarf/src/pkg/state"

	addonsv1alpha1 "github.com/colonel-byte/cluster-api-addon-provider-zarf/api/v1alpha1"
	"github.com/colonel-byte/cluster-api-addon-provider-zarf/internal/resolver"
)

// dockerConfigJSONKey is the key a kubernetes.io/dockerconfigjson secret carries its config
// under - what an operator gets from `kubectl create secret docker-registry`.
const dockerConfigJSONKey = ".dockerconfigjson"

// caBundleKeys are the keys a CA bundle is looked for under, in order, before falling back to
// a single-keyed object's only key. ca.crt is both cert-manager's and Zarf's own.
var caBundleKeys = []string{"ca.crt", "ca.pem", "tls.crt"}

// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch

// registryAccess fills in opts' registry reachability - the credentials and CA a package source
// names - resolving cross-namespace per --secret-namespaces. A refusal comes back as a
// (reason, message) pair for the caller to surface as the same condition a resolution failure
// uses; the error return is a transient API failure worth a retry.
func (r *ZarfPackageProxyReconciler) registryAccess(ctx context.Context, namespace string, pkg *addonsv1alpha1.ZarfPackageSource, opts *resolver.Options) (string, string, error) {
	opts.InsecureSkipTLSVerify = pkg.InsecureSkipTLSVerify

	if pkg.FromZarfState {
		// The management cluster's own Zarf state, not a workload cluster's: the registry being
		// pulled *from* is the one in this cluster (choice-registry-credentials.md).
		return r.zarfStateAccess(ctx, pkg.Repository, opts)
	}

	if ref := pkg.PullSecretRef; ref != nil {
		key, err := ref.Key(namespace, r.SecretNamespaces, "--secret-namespaces")
		if err != nil {
			return addonsv1alpha1.ReasonCrossNamespaceNotEnabled, err.Error(), nil
		}
		secret := &corev1.Secret{}
		if reason, message, err := r.getRegistryObject(ctx, key, secret, "pullSecretRef"); reason != "" || err != nil {
			return reason, message, err
		}
		config, ok := secret.Data[dockerConfigJSONKey]
		if !ok || len(config) == 0 {
			return addonsv1alpha1.ReasonResolutionFailed,
				fmt.Sprintf("secret %s carries no %s key", key, dockerConfigJSONKey), nil
		}
		opts.DockerConfigJSON = config
	}

	if ref := pkg.CABundleSecretRef; ref != nil {
		key, err := ref.Key(namespace, r.SecretNamespaces, "--secret-namespaces")
		if err != nil {
			return addonsv1alpha1.ReasonCrossNamespaceNotEnabled, err.Error(), nil
		}
		secret := &corev1.Secret{}
		if reason, message, err := r.getRegistryObject(ctx, key, secret, "caBundleSecretRef"); reason != "" || err != nil {
			return reason, message, err
		}
		bundle, err := caBundleFrom(secret.Data)
		if err != nil {
			return addonsv1alpha1.ReasonResolutionFailed, fmt.Sprintf("secret %s: %s", key, err), nil
		}
		opts.CABundle = bundle
	}

	if ref := pkg.CABundleConfigMapRef; ref != nil {
		key, err := ref.Key(namespace, r.SecretNamespaces, "--secret-namespaces")
		if err != nil {
			return addonsv1alpha1.ReasonCrossNamespaceNotEnabled, err.Error(), nil
		}
		configMap := &corev1.ConfigMap{}
		if reason, message, err := r.getRegistryObject(ctx, key, configMap, "caBundleConfigMapRef"); reason != "" || err != nil {
			return reason, message, err
		}
		data := make(map[string][]byte, len(configMap.Data)+len(configMap.BinaryData))
		for name, value := range configMap.Data {
			data[name] = []byte(value)
		}
		for name, value := range configMap.BinaryData {
			data[name] = value
		}
		bundle, err := caBundleFrom(data)
		if err != nil {
			return addonsv1alpha1.ReasonResolutionFailed, fmt.Sprintf("configmap %s: %s", key, err), nil
		}
		opts.CABundle = bundle
	}

	return "", "", nil
}

// getRegistryObject reads obj, turning a missing one into the (reason, message) refusal the
// caller surfaces rather than an error that would retry forever against a typo.
func (r *ZarfPackageProxyReconciler) getRegistryObject(ctx context.Context, key types.NamespacedName, obj client.Object, field string) (string, string, error) {
	if err := r.Get(ctx, key, obj); err != nil {
		if apierrors.IsNotFound(err) {
			return addonsv1alpha1.ReasonResolutionFailed,
				fmt.Sprintf("%s names %s, which does not exist", field, key), nil
		}
		return "", "", err
	}
	return "", "", nil
}

// caBundleFrom picks the CA out of a secret's or configmap's data: a conventional key, or the
// only key when there is exactly one. The second case is the one that matters - trust-manager
// and friends let an operator name the key, and refusing a single-keyed bundle because it is
// called something else would be pedantry.
func caBundleFrom(data map[string][]byte) ([]byte, error) {
	for _, name := range caBundleKeys {
		if value, ok := data[name]; ok && len(value) > 0 {
			return value, nil
		}
	}
	if len(data) == 1 {
		for _, value := range data {
			if len(value) > 0 {
				return value, nil
			}
		}
	}
	return nil, fmt.Errorf("no CA bundle under %s, and it has %d keys rather than one to fall back to",
		strings.Join(caBundleKeys, ", "), len(data))
}

// zarfStateAccess reads the registry credentials and CA out of this cluster's own Zarf state -
// the common air-gapped arrangement, where the registry the fleet pulls from is the one Zarf
// already generated credentials and a certificate for.
func (r *ZarfPackageProxyReconciler) zarfStateAccess(ctx context.Context, repository string, opts *resolver.Options) (string, string, error) {
	secret := &corev1.Secret{}
	key := types.NamespacedName{Namespace: zarfstate.ZarfNamespaceName, Name: zarfstate.ZarfStateSecretName}
	if err := r.Get(ctx, key, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return addonsv1alpha1.ReasonResolutionFailed,
				fmt.Sprintf("fromZarfState is set but %s does not exist - this cluster is not zarf-initialized", key), nil
		}
		return "", "", err
	}
	raw, ok := secret.Data[zarfstate.ZarfStateDataKey]
	if !ok {
		return addonsv1alpha1.ReasonResolutionFailed,
			fmt.Sprintf("secret %s carries no %s key", key, zarfstate.ZarfStateDataKey), nil
	}
	state := &zarfstate.State{}
	if err := json.Unmarshal(raw, state); err != nil {
		return addonsv1alpha1.ReasonResolutionFailed,
			fmt.Sprintf("secret %s does not hold zarf state: %s", key, err), nil
	}

	registry := state.RegistryInfo
	username, password := registry.PullUsername, registry.PullPassword
	if username == "" {
		// Zarf's own note on the field: an external registry may carry only the push user.
		username, password = registry.PushUsername, registry.PushPassword
	}
	if username == "" {
		return addonsv1alpha1.ReasonResolutionFailed,
			fmt.Sprintf("zarf state in %s names no registry user", key), nil
	}
	// Keyed on the repository's host rather than the address in state: state records the
	// address a *node* reaches the registry at, which is not what this controller dials.
	opts.DockerConfigJSON = dockerConfigJSON(registryHost(repository), username, password)

	// The CA is there whenever the registry serves TLS Zarf issued. Absent is not a refusal -
	// a registry Zarf did not put a certificate in front of needs no CA.
	tlsSecret := &corev1.Secret{}
	tlsKey := types.NamespacedName{Namespace: zarfstate.ZarfNamespaceName, Name: zarfstate.RegistryServerTLSSecret}
	switch err := r.Get(ctx, tlsKey, tlsSecret); {
	case err == nil:
		opts.CABundle = tlsSecret.Data[zarfstate.RegistrySecretCAPath]
	case apierrors.IsNotFound(err):
	default:
		return "", "", err
	}
	return "", "", nil
}

// registryHost is the registry part of an OCI repository reference - everything before the
// first slash, which is how a Docker config's auths map is keyed.
func registryHost(repository string) string {
	host, _, _ := strings.Cut(repository, "/")
	return host
}

// dockerConfigJSON lays one credential out in the shape oras' credential store reads. Formatted
// rather than marshalled: the document is two strings in a fixed shape, and %q is the same
// escaping a JSON encoder would apply to them.
func dockerConfigJSON(host, username, password string) []byte {
	auth := base64.StdEncoding.EncodeToString([]byte(username + ":" + password))
	return []byte(fmt.Sprintf(`{"auths":{%q:{"auth":%q}}}`, host, auth))
}
