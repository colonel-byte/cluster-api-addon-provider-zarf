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

// Package e2e stands up and tears down the end-to-end topology from
// dist/e2e-test-plan.md (to become docs/dev prose with the first e2e PR): a kind management
// cluster with the docker socket mounted, CAPI core + CAPD initialized by a pinned clusterctl,
// one CAPD workload cluster, and a registry:2 container on the kind docker network reachable as
// one stable name from management pods, deploy pods, and workload nodes.
//
// It is a dev/CI-only surface: it needs docker, kind, and kubectl on PATH plus network access,
// none of which the offline release build may assume. The spike assertions themselves live in
// the separate test/e2e module and run through the e2e:run target.
package e2e

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/magefile/mage/sh"

	"github.com/colonel-byte/cluster-api-addon-provider-zarf/magefiles/pkg/release"
)

const (
	mgmtCluster     = "caapz-e2e"
	workloadCluster = "e2e-workload"

	// registryHost is the one repository-string hostname everything uses: controller pods,
	// deploy pods, workload nodes. HTTP, because the network is test-only.
	registryHost     = "caapz-e2e-registry:5000"
	registryName     = "caapz-e2e-registry"
	registryHostPort = "127.0.0.1:5001"

	workDir = "dist/e2e"

	// clusterAPIVersion pins clusterctl and the CAPD provider together; it deliberately
	// matches the sigs.k8s.io/cluster-api/api pin in go.mod so the e2e surface and the
	// controller speak the same contract.
	//
	// renovate: datasource=github-releases depName=kubernetes-sigs/cluster-api extractVersion=^v(?<version>\d+\.\d+\.\d+)$
	clusterAPIVersion = "1.14.4"

	// workloadK8sVersion is the Kubernetes version CAPD boots in workload clusters. CAPD
	// resolves it to `kindest/node:v<version>`, so the only usable values are versions kind
	// has actually published a node image for - which trails upstream Kubernetes by weeks to
	// months.
	//
	// So this tracks kind's node images, not Kubernetes releases. Tracking releases is what it
	// did until a from-scratch `e2e:up` failed on 1.37.2: there is no kindest/node:v1.37.2, so
	// CAPD created the Cluster, reported InfrastructureReady, and then sat with a machine that
	// could never boot until the ControlPlaneInitialized wait timed out. Nothing said "no such
	// image". It stayed hidden because `ensureWorkloadCluster` caches the generated manifest in
	// the workdir, so every topology built on an existing workdir kept using the version that
	// was valid when that file was written.
	//
	// renovate: datasource=docker depName=kindest/node extractVersion=^v(?<version>\d+\.\d+\.\d+)$
	workloadK8sVersion = "1.37.0"

	// calicoVersion pins the CNI the workload cluster gets. CAPD ships none, so without it the
	// node stays NotReady with "cni plugin not initialized" and nothing can be scheduled -
	// which is invisible until something tries. Calico because the Cluster API quick start
	// pairs it with CAPD: its default pod CIDR is the 192.168.0.0/16 the development flavor
	// hands out, so the manifest applies with nothing to reconfigure.
	//
	// renovate: datasource=github-releases depName=projectcalico/calico extractVersion=^v(?<version>\d+\.\d+\.\d+)$
	calicoVersion = "3.33.1"

	// localPathVersion pins the storage provisioner the workload cluster gets. CAPD creates
	// clusters with no StorageClass at all, so anything asking for a volume there waits
	// forever - the Zarf init package's registry among them, and its chart exposes no way to
	// run without one. This is the same provisioner kind installs in its own clusters by
	// default, which is the shape the rest of the world assumes.
	//
	// renovate: datasource=github-releases depName=rancher/local-path-provisioner extractVersion=^v(?<version>\d+\.\d+\.\d+)$
	localPathVersion = "0.0.37"

	// Image digests below are owned by a written procedure rather than Renovate - the Go
	// custom manager tracks versions, not digests. To bump one:
	//   docker buildx imagetools inspect <image:tag> | grep Digest
	// and update tag and digest together.
	kindNodeImage = "kindest/node:v1.34.0@sha256:7416a61b42b1662ca6ca89f02028ac133a309a2a30ba309614e8ec94d976dc5a"
	registryImage = "ghcr.io/distribution/distribution:3@sha256:d106962e6fe3fa69c178cec77eeeee5edb626b8df6535b7bd6b95e658a0a5ca9"
	spikeImage    = "busybox:1.37@sha256:a7dbc319b1438115a207e11930dc4bb3e643dcb3ec0bdd694bb36cd8fad289ec"

	// providerWaitTimeout is how long each provider namespace gets to report Available. See
	// waitProviders for why it is this long rather than the five minutes it was.
	providerWaitTimeout = "15m"

	// controllerImage is the locally built controller image e2e:run kind-loads - never pushed
	// anywhere, so the tag needs no pin owner. Not :latest, so the kubelet's IfNotPresent
	// default finds the loaded image instead of pulling.
	controllerImage = "caapz-e2e/controller:dev"
)

// mgmtKubeconfig is where Up writes the management cluster's kubeconfig; e2e:run hands it to the
// test module as E2E_KUBECONFIG.
func mgmtKubeconfig() string { return filepath.Join(workDir, "mgmt.kubeconfig") }

// workloadKubeconfig is where Up writes the workload cluster's kubeconfig, as `clusterctl get
// kubeconfig` rewrites it for host access - the localhost/port-map variant, not the pod-reachable
// one in the Cluster's secret. e2e:run hands it to the test module as E2E_WORKLOAD_KUBECONFIG so
// assertions can read what a deploy actually left on the target cluster.
func workloadKubeconfig() string { return filepath.Join(workDir, "workload.kubeconfig") }

// kindConfig renders the management cluster's kind config: the pinned node image, the docker
// socket mount CAPD requires to create sibling containers, and a containerd config_path so
// per-registry hosts.toml files work.
func kindConfig() string {
	return fmt.Sprintf(`kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
  - role: control-plane
    image: %s
    extraMounts:
      - hostPath: /var/run/docker.sock
        containerPath: /var/run/docker.sock
containerdConfigPatches:
  - |-
    [plugins."io.containerd.grpc.v1.cri".registry]
      config_path = "/etc/containerd/certs.d"
`, kindNodeImage)
}

// hostsToml renders the containerd hosts.toml that lets a node pull from the test registry by
// its stable name over plain HTTP.
func hostsToml() string {
	return fmt.Sprintf("[host.\"http://%s\"]\n  capabilities = [\"pull\", \"resolve\"]\n", registryHost)
}

// Up brings the whole topology up. Every step is idempotent, so a failed run is re-runnable.
func Up() error {
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return err
	}
	steps := []struct {
		name string
		run  func() error
	}{
		{"clusterctl binary", ensureClusterctl},
		{"registry container", ensureRegistry},
		{"secure registry container", ensureSecureRegistry},
		{"kind management cluster", ensureMgmtCluster},
		{"registry wiring on management nodes", configureMgmtRegistry},
		{"CAPI core + CAPD", clusterctlInit},
		{"provider deployments available", waitProviders},
		{"workload cluster", ensureWorkloadCluster},
		{"registry wiring on workload nodes", configureWorkloadRegistry},
		{"CNI on the workload cluster", installWorkloadCNI},
		{"spike image in registry", seedSpikeImage},
		{"fixture signing keypairs", ensureKeypairs},
		{"fixture packages in registry", seedFixturePackages},
		{"app fixture in the secure registry", seedSecureRegistry},
		{"init package mirror", mirrorInitPackage},
		{"zarf in the management cluster", zarfInitManagement},
		{"app fixture in the management cluster's zarf registry", seedManagementZarfRegistry},
	}
	for _, step := range steps {
		fmt.Println("==>", step.name)
		if err := step.run(); err != nil {
			return fmt.Errorf("%s: %w", step.name, err)
		}
	}
	fmt.Println("e2e topology is up; run the suite with e2e:run")
	return nil
}

// Down tears everything down, tolerating a partial or absent topology. The CAPD machine
// containers are siblings the management cluster created, so deleting the management cluster
// orphans them - they are removed by their kind cluster label.
func Down() error {
	bestEffort("kind", "delete", "cluster", "--name", mgmtCluster)
	bestEffort("docker", "rm", "-f", registryName)
	bestEffort("docker", "rm", "-f", secureRegistryName)
	orphans, err := sh.Output("docker", "ps", "-aq", "--filter", "label=io.x-k8s.kind.cluster="+workloadCluster)
	if err != nil {
		return err
	}
	for _, id := range strings.Fields(orphans) {
		bestEffort("docker", "rm", "-f", id)
	}
	return nil
}

// bestEffort runs a teardown command whose failure means "already gone", which Down tolerates.
func bestEffort(cmd string, args ...string) {
	if err := sh.RunV(cmd, args...); err != nil {
		fmt.Println("teardown: ignoring:", err)
	}
}

// Run builds and installs the controller into the running topology, then executes the suite in
// the separate test/e2e module against it. It does not depend on Up so a developer can iterate
// on code and tests against a long-lived topology - every run reinstalls the freshly built
// controller; it refuses with a pointer at e2e:up when the topology is absent.
func Run() error {
	kubeconfig, err := filepath.Abs(mgmtKubeconfig())
	if err != nil {
		return err
	}
	if _, err := os.Stat(kubeconfig); err != nil {
		return fmt.Errorf("no management kubeconfig at %s - run e2e:up first", kubeconfig)
	}
	workload, err := filepath.Abs(workloadKubeconfig())
	if err != nil {
		return err
	}
	if _, err := os.Stat(workload); err != nil {
		return fmt.Errorf("no workload kubeconfig at %s - run e2e:up first", workload)
	}
	steps := []struct {
		name string
		run  func() error
	}{
		{"controller image", buildControllerImage},
		{"controller image into kind", loadControllerImage},
		{"controller manifests", installController},
	}
	for _, step := range steps {
		fmt.Println("==>", step.name)
		if err := step.run(); err != nil {
			return fmt.Errorf("%s: %w", step.name, err)
		}
	}
	keys, err := filepath.Abs(keyDir())
	if err != nil {
		return err
	}
	secure, err := filepath.Abs(secureRegistryDir())
	if err != nil {
		return err
	}
	return sh.RunWithV(map[string]string{
		"E2E_KUBECONFIG":          kubeconfig,
		"E2E_WORKLOAD_KUBECONFIG": workload,
		// The fixture keypairs, so a test can name the public half a proxy verifies against
		// and the unrelated one the bad-signature case needs.
		"E2E_KEY_DIR": keys,
		// The secure registry's generated CA, which the registry-credential tests put in the
		// Secret and ConfigMap a proxy names. Generated per topology, so it cannot be a
		// constant in the suite the way the hostname and the password are.
		"E2E_SECURE_REGISTRY_DIR": secure,
	}, "go", "-C", "test/e2e", "test", "-count=1", "-timeout=40m", "./...")
}

// buildControllerImage builds the e2e controller image into the e2e workdir with the release
// recipe (release.BuildImage), so the e2e image and the released image stay the same recipe.
func buildControllerImage() error {
	return release.BuildImage(controllerImage, filepath.Join(workDir, "image"))
}

func loadControllerImage() error {
	return sh.RunV("kind", "load", "docker-image", controllerImage, "--name", mgmtCluster)
}

// controllerKustomization renders the e2e overlay over config/default: the locally built image,
// and --registry-plain-http because the test registry speaks HTTP. The overlay lives in the
// workdir so the working tree is never mutated the way release:manifests' kustomize-edit is.
// The image match is the release reference, not manager.yaml's "controller" placeholder:
// config/manager's own images transformer has already rewritten the placeholder by the time an
// overlay transformer sees the output.
func controllerKustomization() string {
	return fmt.Sprintf(`apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - ../../../config/default
images:
  - name: %s
    newName: caapz-e2e/controller
    newTag: dev
patches:`, release.ControllerImage) + `
  - target:
      kind: Deployment
      name: controller-manager
    patch: |-
      - op: add
        path: /spec/template/spec/containers/0/args/-
        value: --registry-plain-http
`
}

// installController applies the rendered controller manifests and waits for the rollout.
func installController() error {
	overlayDir := filepath.Join(workDir, "controller")
	if err := os.MkdirAll(overlayDir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(overlayDir, "kustomization.yaml"),
		[]byte(controllerKustomization()), 0o644); err != nil { //nolint:gosec // manifest
		return err
	}
	manifestPath := filepath.Join(workDir, "controller.yaml")
	if err := sh.RunV("go", "tool", "kustomize", "build", overlayDir, "-o", manifestPath); err != nil {
		return err
	}
	if err := kubectl("apply", "-f", manifestPath); err != nil {
		return err
	}
	// A reinstall with an unchanged manifest changes nothing, so the rollout wait would pass on
	// the old pod still running the old image; restart to force the fresh build in.
	if err := kubectl("-n", "caapz-system", "rollout", "restart", "deployment/caapz-controller-manager"); err != nil {
		return err
	}
	return kubectl("-n", "caapz-system", "rollout", "status", "deployment/caapz-controller-manager", "--timeout=3m")
}

func ensureClusterctl() error {
	path := clusterctlPath()
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	url := fmt.Sprintf(
		"https://github.com/kubernetes-sigs/cluster-api/releases/download/v%s/clusterctl-%s-%s",
		clusterAPIVersion, runtime.GOOS, runtime.GOARCH)
	fmt.Println("downloading", url)
	resp, err := http.Get(url) //nolint:gosec,noctx // pinned release URL, dev-only target
	if err != nil {
		return err
	}
	body, err := io.ReadAll(resp.Body)
	if closeErr := resp.Body.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return os.WriteFile(path, body, 0o755) //nolint:gosec // executable
}

func clusterctlPath() string { return filepath.Join(workDir, "clusterctl") }

func ensureRegistry() error {
	// An inspect error just means the container is absent; recreate in every non-running case.
	if running, err := sh.Output("docker", "inspect", "-f", "{{.State.Running}}", registryName); err == nil &&
		strings.TrimSpace(running) == "true" {
		return nil
	}
	bestEffort("docker", "rm", "-f", registryName)
	return sh.RunV("docker", "run", "-d", "--restart=always",
		"-p", registryHostPort+":5000", "--name", registryName, registryImage)
}

func ensureMgmtCluster() error {
	clusters, err := sh.Output("kind", "get", "clusters")
	if err != nil {
		return err
	}
	if !slices.Contains(strings.Fields(clusters), mgmtCluster) {
		configPath := filepath.Join(workDir, "kind.yaml")
		if err := os.WriteFile(configPath, []byte(kindConfig()), 0o644); err != nil { //nolint:gosec // config file
			return err
		}
		if err := sh.RunV("kind", "create", "cluster", "--name", mgmtCluster, "--config", configPath); err != nil {
			return err
		}
	}
	kubeconfig, err := sh.Output("kind", "get", "kubeconfig", "--name", mgmtCluster)
	if err != nil {
		return err
	}
	// The registry joins the kind docker network so one name resolves everywhere. "already
	// exists" is the idempotent case.
	if err := sh.RunV("docker", "network", "connect", "kind", registryName); err != nil {
		connected, inspectErr := sh.Output("docker", "network", "inspect", "kind",
			"-f", "{{range .Containers}}{{.Name}} {{end}}")
		if inspectErr != nil || !strings.Contains(connected, registryName) {
			return err
		}
	}
	return os.WriteFile(mgmtKubeconfig(), []byte(kubeconfig), 0o600)
}

// configureMgmtRegistry writes hosts.toml on every management node.
func configureMgmtRegistry() error {
	nodes, err := sh.Output("kind", "get", "nodes", "--name", mgmtCluster)
	if err != nil {
		return err
	}
	return writeHostsToml(strings.Fields(nodes))
}

// configureWorkloadRegistry writes hosts.toml on every CAPD machine container, found by the kind
// cluster label CAPD stamps on them.
func configureWorkloadRegistry() error {
	// Role-filtered: the cluster label also matches CAPD's haproxy load balancer, which runs
	// no containerd and has no shell to exec.
	var nodes []string
	for _, role := range []string{"control-plane", "worker"} {
		out, err := sh.Output("docker", "ps", "-q",
			"--filter", "label=io.x-k8s.kind.cluster="+workloadCluster,
			"--filter", "label=io.x-k8s.kind.role="+role)
		if err != nil {
			return err
		}
		nodes = append(nodes, strings.Fields(out)...)
	}
	if len(nodes) == 0 {
		return fmt.Errorf("no running node containers labeled io.x-k8s.kind.cluster=%s", workloadCluster)
	}
	if err := writeHostsToml(nodes); err != nil {
		return err
	}
	// Unlike the management nodes, CAPD machines never saw kind's containerdConfigPatches, so
	// their containerd (2.x: the cri.v1.images plugin) has no config_path and ignores
	// hosts.toml. Append it once and restart containerd.
	patch := "\n[plugins.\"io.containerd.cri.v1.images\".registry]\n  config_path = \"/etc/containerd/certs.d\"\n"
	for _, node := range nodes {
		script := fmt.Sprintf(
			"grep -q 'config_path' /etc/containerd/config.toml || { printf '%%s' '%s' >> /etc/containerd/config.toml && systemctl restart containerd; }",
			patch)
		if err := sh.RunV("docker", "exec", node, "sh", "-c", script); err != nil {
			return err
		}
	}
	return nil
}

func writeHostsToml(containers []string) error {
	dir := "/etc/containerd/certs.d/" + registryHost
	for _, node := range containers {
		script := fmt.Sprintf("mkdir -p %s && printf '%%s' '%s' > %s/hosts.toml", dir, hostsToml(), dir)
		if err := sh.RunV("docker", "exec", node, "sh", "-c", script); err != nil {
			return err
		}
	}
	return nil
}

func clusterctlInit() error {
	if err := kubectl("get", "namespace", "capd-system"); err == nil {
		return nil
	}
	// CLUSTER_TOPOLOGY enables ClusterClass, which CAPD's development flavor templates use.
	return sh.RunWithV(map[string]string{"CLUSTER_TOPOLOGY": "true"},
		clusterctlPath(), "init", "--infrastructure", "docker:v"+clusterAPIVersion,
		"--kubeconfig", mgmtKubeconfig())
}

// waitProviders blocks until every provider deployment is Available - applying a workload
// cluster before the webhooks serve fails with connection-refused, which a fresh `clusterctl
// init` otherwise races.
//
// The timeout is sized for a cold start, not a warm one. A kind cluster's image cache goes with
// the cluster, so a topology built from nothing pulls every CAPI provider image fresh from
// registry.k8s.io - four of them, and the pods sit in ContainerCreating until the last one
// lands. Measured at seven to eight minutes on a residential connection, against the five this
// used to allow, and the failure reads as "timed out waiting for the condition" with pods that
// are simply still pulling. Warm runs are unaffected: the wait returns as soon as the
// deployments are Available.
func waitProviders() error {
	for _, ns := range []string{
		"capi-system",
		"capi-kubeadm-bootstrap-system",
		"capi-kubeadm-control-plane-system",
		"capd-system",
	} {
		if err := kubectl("wait", "--for=condition=Available", "deployment", "--all",
			"-n", ns, "--timeout="+providerWaitTimeout); err != nil {
			return err
		}
	}
	return nil
}

func ensureWorkloadCluster() error {
	if err := kubectl("get", "cluster.cluster.x-k8s.io", workloadCluster); err != nil {
		// Keyed on the version, so changing workloadK8sVersion regenerates rather than
		// silently reusing a manifest pinned to the old one. An unkeyed cache is how a bad
		// version pin stayed invisible: every topology built on an existing workdir kept
		// booting the version that was valid when the file was written.
		manifestPath := filepath.Join(workDir, "workload-v"+workloadK8sVersion+".yaml")
		// Reuse a previously generated manifest: `clusterctl generate` validates against
		// live objects, so after a partially applied run it refuses to regenerate what it
		// half-created - the cached manifest is what makes this step resumable.
		if _, err := os.Stat(manifestPath); err != nil {
			manifest, err := sh.Output(clusterctlPath(), "generate", "cluster", workloadCluster,
				"--infrastructure", "docker:v"+clusterAPIVersion,
				"--kubernetes-version", "v"+workloadK8sVersion,
				"--control-plane-machine-count", "1",
				"--worker-machine-count", "0",
				"--flavor", "development",
				"--kubeconfig", mgmtKubeconfig())
			if err != nil {
				return err
			}
			if err := os.WriteFile(manifestPath, []byte(manifest), 0o644); err != nil { //nolint:gosec // manifest
				return err
			}
		}
		// Deployment Available does not mean the providers' webhook endpoints serve yet,
		// and there is nothing cleaner to wait on - retry the apply through the gap.
		deadline := time.Now().Add(3 * time.Minute)
		for {
			err := kubectl("apply", "-f", manifestPath)
			if err == nil {
				break
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("applying the workload manifest never cleared the provider webhooks: %w", err)
			}
			fmt.Println("webhooks not serving yet; retrying apply")
			time.Sleep(10 * time.Second)
		}
	}
	if err := kubectl("wait", "cluster.cluster.x-k8s.io/"+workloadCluster,
		"--for=condition=ControlPlaneInitialized", "--timeout=10m"); err != nil {
		return err
	}
	// The kubeconfig secret follows initialization closely; poll briefly rather than wait.
	deadline := time.Now().Add(2 * time.Minute)
	for {
		if err := kubectl("get", "secret", workloadCluster+"-kubeconfig"); err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("secret %s-kubeconfig never appeared", workloadCluster)
		}
		time.Sleep(5 * time.Second)
	}
}

// installWorkloadCNI writes the workload cluster's host-reachable kubeconfig and applies Calico
// into it, then waits for the node to go Ready. CAPD creates clusters with no CNI at all, so
// until this runs the node reports "cni plugin not initialized", coredns sits Pending, and
// nothing - least of all a zarf init - can be scheduled.
func installWorkloadCNI() error {
	kubeconfig, err := sh.Output(clusterctlPath(), "get", "kubeconfig", workloadCluster,
		"--kubeconfig", mgmtKubeconfig())
	if err != nil {
		return err
	}
	if err := os.WriteFile(workloadKubeconfig(), []byte(kubeconfig), 0o600); err != nil {
		return err
	}
	// Idempotent by Calico's own account: a re-apply of the same manifest is a no-op, so the
	// step needs no "already installed" probe of its own.
	manifest := fmt.Sprintf(
		"https://raw.githubusercontent.com/projectcalico/calico/v%s/manifests/calico.yaml", calicoVersion)
	if err := workloadKubectl("apply", "-f", manifest); err != nil {
		return err
	}
	if err := workloadKubectl("wait", "node", "--all",
		"--for=condition=Ready", "--timeout=10m"); err != nil {
		// Ready is the assertion that matters - the apply succeeding only means the objects exist.
		return err
	}

	// The init package's registry is pinned to infrastructure nodes by a required node
	// affinity on this label, so without it the registry pod stays Pending and the init
	// never finishes. A one-node test cluster has nowhere else to put it.
	if err := workloadKubectl("label", "node", "--all", "--overwrite", infraNodeLabel+"=true"); err != nil {
		return err
	}

	// Zarf's injector bootstraps the seed registry by reusing an image already present on a
	// node, and it considers a node with any NoSchedule or NoExecute taint ineligible. CAPD
	// gives its control-plane machine the standard control-plane taint, so on a cluster whose
	// only machine is that one, injection fails with "no suitable injector image or node
	// exists" and no init can ever complete. Removing the taint is what kind already does for
	// its own single-node clusters; the alternative is paying for a worker machine the rest of
	// the suite has no use for.
	//
	// Best-effort: removing a taint that is already gone is an error, which is the ordinary
	// case on every re-run of Up against a standing topology.
	if err := workloadKubectl("taint", "node", "--all",
		"node-role.kubernetes.io/control-plane:NoSchedule-", "--overwrite"); err != nil {
		fmt.Println("the control-plane taint was already removed")
	}

	// A StorageClass, for the same reason: CAPD ships none, and the init package's registry
	// chart offers no way to run without a volume.
	storageManifest := fmt.Sprintf(
		"https://raw.githubusercontent.com/rancher/local-path-provisioner/v%s/deploy/local-path-storage.yaml",
		localPathVersion)
	if err := workloadKubectl("apply", "-f", storageManifest); err != nil {
		return err
	}
	// The provisioner creates each volume through a helper pod that mounts a hostPath, and the
	// CAPD cluster enforces the baseline Pod Security standard, which forbids exactly that -
	// so provisioning fails inside the provisioner with "violates PodSecurity", the claim
	// stays Pending, and the pod waiting on it never schedules. kind's own clusters enforce
	// nothing, which is why this only bites here.
	if err := workloadKubectl("label", "namespace", "local-path-storage", "--overwrite",
		"pod-security.kubernetes.io/enforce=privileged"); err != nil {
		return err
	}
	// Default, so a claim naming no class still binds - which is how the registry's chart
	// writes its PVC.
	return workloadKubectl("patch", "storageclass", "local-path", "-p",
		`{"metadata":{"annotations":{"storageclass.kubernetes.io/is-default-class":"true"}}}`)
}

// seedSpikeImage stages the spike-pod image into the local registry, the same
// staged-ahead-of-time motion an air-gapped environment uses for real packages.
func seedSpikeImage() error {
	target := registryHostPort + "/spike/busybox:test"
	if err := sh.RunV("docker", "pull", spikeImage); err != nil {
		return err
	}
	if err := sh.RunV("docker", "tag", spikeImage, target); err != nil {
		return err
	}
	return sh.RunV("docker", "push", target)
}

func kubectl(args ...string) error {
	return sh.RunV("kubectl", append([]string{"--kubeconfig", mgmtKubeconfig()}, args...)...)
}

func workloadKubectl(args ...string) error {
	return sh.RunV("kubectl", append([]string{"--kubeconfig", workloadKubeconfig()}, args...)...)
}
