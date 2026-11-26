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
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/magefile/mage/sh"
	zarflog "github.com/zarf-dev/zarf/src/pkg/logger"
	"github.com/zarf-dev/zarf/src/pkg/packager"
	"github.com/zarf-dev/zarf/src/pkg/packager/filters"
	"github.com/zarf-dev/zarf/src/pkg/packager/layout"
	zarfstate "github.com/zarf-dev/zarf/src/pkg/state"
	"github.com/zarf-dev/zarf/src/types"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/retry"
	"sigs.k8s.io/yaml"
)

// Zarf in the management cluster, which is what `spec.package.fromZarfState` reads. The field
// exists for the common air-gapped arrangement - the fleet pulls from a Zarf registry that Zarf
// itself installed, in the cluster the controller runs in - and nothing short of a real one
// proves the controller reads the right cluster's state, keys the credential on the right host,
// and hands the CA on to the deploy pod.
//
// This is the one step in the topology that changes the management cluster rather than adding
// to it, so the guard rails are in the code rather than in a comment: see
// ensureAgentIgnoreLabels.
const (
	// agentLabel is the label Zarf's agent webhook excludes namespaces and objects by.
	agentLabel = "zarf.dev/agent"

	// mgmtInjectorImage is the image Zarf's injector pod runs. Proxy mode takes it as an
	// argument rather than discovering an image already on a node, so it has to name one the
	// node already holds - the spike busybox, staged by seedSpikeImage. Busybox rather than the
	// controller image, which is a chainguard/static base with no shell running as 65532:
	// zarf's injector mounts a binary into whatever image this names and expects to exec it.
	mgmtInjectorImage = registryHost + "/spike/busybox:test"

	// zarfRegistryForwardPort is the local port the fixture is pushed through. The registry
	// certificate names 127.0.0.1, so a forwarded push verifies against the same CA a proxy
	// uses.
	zarfRegistryForwardPort = "5003"
)

// zarfInitComponents is the init-slim selection, the same four the auto-init e2e proxy names.
// init-slim marks every component optional, so an empty selection deploys nothing.
var zarfInitComponents = []string{"zarf-injector", "zarf-seed-registry", "zarf-registry", "zarf-agent"}

// ensureAgentIgnoreLabels labels every namespace that already exists `zarf.dev/agent=ignore`,
// and must run before the init deploys the agent.
//
// The agent's pod webhook is `failurePolicy: Fail` over every pod CREATE in the cluster. Its
// namespace selector already excludes kube-system by name, so the CNI and the rest of the
// control plane are never at risk; what is at risk is capi-system, capd-system, caapz-system,
// cert-manager and `default` - where the deploy Jobs run - whose images it would rewrite to a
// registry that does not hold them.
//
// Two mechanisms, because they fail differently. The init sets MutationPolicyLabeled, so the
// agent mutates nothing that has not asked to be mutated - that is the one that keeps images
// intact. This label is the other half: an ignored namespace is excluded by the webhook's own
// selector, so the webhook is never *called* for it, and an unhealthy agent cannot block the
// controller or a deploy Job from starting. Policy alone would leave that failure mode open.
//
// Namespaces created *after* this runs are not covered, which is deliberate rather than
// overlooked - a package deploying into a fresh namespace is a case where the agent should act,
// and the deploy Jobs that must not be touched all run in `default`.
func ensureAgentIgnoreLabels() error {
	raw, err := sh.Output("kubectl", "--kubeconfig", mgmtKubeconfig(), "get", "namespaces",
		"-o", "jsonpath={range .items[*]}{.metadata.name}{\"\\n\"}{end}")
	if err != nil {
		return err
	}
	for _, namespace := range strings.Fields(raw) {
		if namespace == zarfstate.ZarfNamespaceName {
			// Zarf labels its own, and labelling it ignore would stop it managing its secrets.
			continue
		}
		if err := kubectl("label", "namespace", namespace, agentLabel+"=ignore", "--overwrite"); err != nil {
			return err
		}
	}

	// Verified rather than assumed: a namespace that silently missed the label is a broken
	// management cluster ten minutes later, with the cause nowhere near the symptom.
	for _, namespace := range strings.Fields(raw) {
		if namespace == zarfstate.ZarfNamespaceName {
			continue
		}
		value, err := sh.Output("kubectl", "--kubeconfig", mgmtKubeconfig(), "get", "namespace",
			namespace, "-o", "jsonpath={.metadata.labels['zarf\\.dev/agent']}")
		if err != nil {
			return err
		}
		if strings.TrimSpace(value) != "ignore" {
			return fmt.Errorf("namespace %s is not labelled %s=ignore (got %q); refusing to deploy the zarf agent into this cluster",
				namespace, agentLabel, value)
		}
	}
	return nil
}

// zarfInitManagement deploys init-slim into the management cluster through the vendored SDK -
// the same LoadPackage/Deploy pair the deploy pod runs. Not through a ZarfPackageProxy, because
// a proxy targets CAPI Cluster objects and the management cluster is not one.
//
// Registry mode is proxy, not the nodeport mode the workload cluster uses, and that is the whole
// reason this exists: `MTLSStrategyZarfManaged` is set only in proxy mode (cluster.go's switch
// hard-forces nodeport to `none`), and without it there is no registry CA in state for
// fromZarfState to find.
func zarfInitManagement() error {
	// The SDK logs through its own context logger and discards without one, which makes a
	// multi-minute init look like a hang.
	ctx := zarflog.WithContext(context.Background(), slog.New(slog.NewTextHandler(os.Stderr, nil)))

	initialized, err := mgmtZarfInitialized()
	if err != nil {
		return err
	}
	if initialized {
		fmt.Println("the management cluster already carries zarf state; skipping")
		return nil
	}

	if err := ensureAgentIgnoreLabels(); err != nil {
		return err
	}
	// init-slim pins its registry to nodes carrying this label with a required node affinity,
	// so without it the registry pod stays Pending and the init never finishes - the same
	// reason the workload cluster's node carries it.
	if err := kubectl("label", "node", "--all", "--overwrite", infraNodeLabel+"=true"); err != nil {
		return err
	}

	// The SDK reads the ambient kubeconfig; everything else in this package passes one
	// explicitly, so point it at the management cluster for this call.
	kubeconfig, err := filepath.Abs(mgmtKubeconfig())
	if err != nil {
		return err
	}
	if err := os.Setenv("KUBECONFIG", kubeconfig); err != nil {
		return err
	}

	ref := registryHostPort + "/" + fixtureRepository + "/" + initPackageName + ":" + initPackageVersion
	loadOpts := packager.LoadOptions{
		CachePath:            filepath.Join(workDir, "zarf-cache"),
		VerificationStrategy: layout.VerifyNever,
		Architecture:         "amd64",
		Filter:               filters.ForDeploy(strings.Join(zarfInitComponents, ",")),
		RemoteOptions:        types.RemoteOptions{PlainHTTP: true},
	}
	pkgLayout, err := packager.LoadPackage(ctx, "oci://"+ref, loadOpts)
	if err != nil {
		return fmt.Errorf("loading %s: %w", ref, err)
	}

	deployOpts := packager.DeployOptions{
		Connectivity: zarfstate.PackageConnectivityAirGap,
		// Proxy mode, for the registry CA. The injector image has to be named with it.
		RegistryInfo:  zarfstate.RegistryInfo{RegistryMode: zarfstate.RegistryModeProxy},
		InjectorImage: mgmtInjectorImage,
		// Opt-in mutation: the agent rewrites only what carries `zarf.dev/agent: mutate`,
		// rather than everything that has not opted out. The management cluster's own
		// infrastructure keeps its image references either way.
		AgentMutationPolicy: zarfstate.MutationPolicyLabeled,
		RemoteOptions:       types.RemoteOptions{PlainHTTP: true},
		SkipVersionCheck:    true,
	}
	// init-slim ships `seedRegistry.persistence.enabled: false` and the upstream registry
	// chart's schema is closed, so the seed registry starts with no storage configuration and
	// crashloops - the same workaround the auto-init e2e proxy carries. kind's own
	// local-path StorageClass is the default here, so the claim binds.
	if err := yaml.Unmarshal([]byte(`{"seedRegistry":{"persistence":{"enabled":true}}}`), &deployOpts.Values); err != nil {
		return err
	}

	fmt.Println("zarf-initializing the management cluster in proxy mode (this takes a few minutes)")
	if _, err := packager.Deploy(ctx, pkgLayout, deployOpts); err != nil {
		return fmt.Errorf("deploying the init package into the management cluster: %w", err)
	}
	return nil
}

// mgmtZarfInitialized reports whether the management cluster already carries zarf state, so Up
// stays re-runnable - this is the one step that cannot simply be repeated.
func mgmtZarfInitialized() (bool, error) {
	err := sh.Run("kubectl", "--kubeconfig", mgmtKubeconfig(), "get", "secret",
		"-n", zarfstate.ZarfNamespaceName, zarfstate.ZarfStateSecretName)
	if err == nil {
		return true, nil
	}
	// A missing secret and a missing namespace are both "not initialized"; anything else would
	// have failed the step before this one.
	return false, nil
}

// mgmtRegistryCredentials reads the push credentials and the registry CA out of the management
// cluster's Zarf state - the same two reads the controller's fromZarfState does, from the host.
func mgmtRegistryCredentials() (username, password string, caPEM []byte, err error) {
	raw, err := sh.Output("kubectl", "--kubeconfig", mgmtKubeconfig(), "get", "secret",
		"-n", zarfstate.ZarfNamespaceName, zarfstate.ZarfStateSecretName,
		"-o", "jsonpath={.data.state}")
	if err != nil {
		return "", "", nil, err
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(raw))
	if err != nil {
		return "", "", nil, err
	}
	state := &zarfstate.State{}
	if err := json.Unmarshal(decoded, state); err != nil {
		return "", "", nil, err
	}

	encodedCA, err := sh.Output("kubectl", "--kubeconfig", mgmtKubeconfig(), "get", "secret",
		"-n", zarfstate.ZarfNamespaceName, zarfstate.RegistryServerTLSSecret,
		"-o", "jsonpath={.data['ca\\.crt']}")
	if err != nil {
		return "", "", nil, fmt.Errorf("reading %s - proxy mode should have written it: %w",
			zarfstate.RegistryServerTLSSecret, err)
	}
	caPEM, err = base64.StdEncoding.DecodeString(strings.TrimSpace(encodedCA))
	if err != nil {
		return "", "", nil, err
	}
	return state.RegistryInfo.PushUsername, state.RegistryInfo.PushPassword, caPEM, nil
}

// seedManagementZarfRegistry copies the app fixture into the management cluster's Zarf registry,
// through a port-forward. oras rather than the Zarf SDK's own publish for the reason the secure
// registry needs it too: publish takes no credentials.
func seedManagementZarfRegistry() error {
	ctx := context.Background()
	username, password, caPEM, err := mgmtRegistryCredentials()
	if err != nil {
		return err
	}

	forward := exec.Command("kubectl", "--kubeconfig", mgmtKubeconfig(), "port-forward", //nolint:gosec // fixed arguments
		"-n", zarfstate.ZarfNamespaceName, "svc/zarf-docker-registry",
		zarfRegistryForwardPort+":5000")
	if err := forward.Start(); err != nil {
		return err
	}
	defer func() {
		//nolint:errcheck // the forward is torn down on the way out; neither result is actionable
		forward.Process.Kill()
		forward.Wait() //nolint:errcheck // reaps the killed child
	}()

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return fmt.Errorf("the registry CA in zarf state is not PEM")
	}
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return fmt.Errorf("http.DefaultTransport is not an *http.Transport")
	}
	transport := base.Clone()
	transport.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}

	local := "127.0.0.1:" + zarfRegistryForwardPort
	reference := fixtureRepository + "/" + appPackage
	dst, err := remote.NewRepository(local + "/" + reference)
	if err != nil {
		return err
	}
	dst.Client = &auth.Client{
		Client:     &http.Client{Transport: retry.NewTransport(transport)},
		Cache:      auth.NewCache(),
		Credential: auth.StaticCredential(local, auth.Credential{Username: username, Password: password}),
	}

	// The forward needs a moment, and the registry answers 401 to an anonymous /v2/ once it is
	// listening - which is the signal that both the tunnel and auth are up.
	if err := waitRegistryReady("https://"+local+"/v2/", transport); err != nil {
		return err
	}
	if _, err := dst.Resolve(ctx, fixtureVersion); err == nil {
		fmt.Println("the app fixture is already in the management cluster's zarf registry; skipping")
		return nil
	}

	src, err := remote.NewRepository(registryHostPort + "/" + reference)
	if err != nil {
		return err
	}
	src.PlainHTTP = true

	fmt.Println("copying", reference, "into the management cluster's zarf registry")
	_, err = oras.Copy(ctx, src, fixtureVersion, dst, fixtureVersion, oras.DefaultCopyOptions)
	return err
}

// waitRegistryReady blocks until url answers at all - 401 included, which is what an
// authenticated registry says to an anonymous request.
func waitRegistryReady(url string, transport *http.Transport) error {
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
	deadline := time.Now().Add(90 * time.Second)
	var last error
	for time.Now().Before(deadline) {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
		if err != nil {
			return err
		}
		resp, err := client.Do(req)
		if err == nil {
			resp.Body.Close() //nolint:errcheck // a readiness probe's body is never read
			return nil
		}
		last = err
		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("%s never answered: %w", url, last)
}
