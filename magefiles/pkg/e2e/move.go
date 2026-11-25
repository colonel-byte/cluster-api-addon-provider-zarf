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
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/magefile/mage/sh"
)

// `clusterctl move` is how an operator migrates a management cluster, and it is the one
// operation that acts on this provider's objects without going through its controller: clusterctl
// reads them, pauses the Cluster, recreates them on the target, and deletes the originals. What
// makes that work for a provider is the `clusterctl.cluster.x-k8s.io/move` label on its CRDs,
// without which its objects are silently left behind - the cluster arrives and nothing it was
// carrying does.
//
// The pivot needs a second management cluster, with its own CAPI, CAPD and CAAPZ install, so it
// is its own target rather than part of e2e:run. The default suite still guards the label on
// every run through a dry run, which needs no target at all; this target is what proves the
// whole motion.

const moveTargetCluster = "caapz-e2e-target"

// moveTargetKubeconfig is where Move writes the target management cluster's kubeconfig; the
// suite reads it as E2E_TARGET_KUBECONFIG and skips the pivot test when it is absent.
func moveTargetKubeconfig() string { return filepath.Join(workDir, "target.kubeconfig") }

// Move stands up a second management cluster, installs this provider into it, and runs the
// pivot test against the pair. It depends on a topology e2e:up already built - the cluster being
// moved, and the packages deployed to it, are what the test asserts survive.
func Move() error {
	source, err := filepath.Abs(mgmtKubeconfig())
	if err != nil {
		return err
	}
	if _, err := os.Stat(source); err != nil {
		return fmt.Errorf("no management kubeconfig at %s - run e2e:up first", source)
	}

	steps := []struct {
		name string
		run  func() error
	}{
		{"target management cluster", ensureMoveTarget},
		{"registry wiring on target nodes", configureTargetRegistry},
		{"CAPI core + CAPD on the target", clusterctlInitTarget},
		{"provider deployments available on the target", waitTargetProviders},
		{"controller image into the target", loadControllerImageInto},
		{"controller manifests on the target", installControllerOnTarget},
	}
	for _, step := range steps {
		fmt.Println("==>", step.name)
		if err := step.run(); err != nil {
			return fmt.Errorf("%s: %w", step.name, err)
		}
	}

	target, err := filepath.Abs(moveTargetKubeconfig())
	if err != nil {
		return err
	}
	clusterctl, err := filepath.Abs(clusterctlPath())
	if err != nil {
		return err
	}
	keys, err := filepath.Abs(keyDir())
	if err != nil {
		return err
	}
	workload, err := filepath.Abs(workloadKubeconfig())
	if err != nil {
		return err
	}
	return sh.RunWithV(map[string]string{
		"E2E_KUBECONFIG":          source,
		"E2E_WORKLOAD_KUBECONFIG": workload,
		"E2E_KEY_DIR":             keys,
		"E2E_CLUSTERCTL":          clusterctl,
		"E2E_TARGET_KUBECONFIG":   target,
	}, "go", "-C", "test/e2e", "test", "-count=1", "-timeout=40m", "-run", "TestClusterctlMove", "./...")
}

// MoveDown removes only the target management cluster, leaving the topology e2e:up built alone.
func MoveDown() error {
	bestEffort("kind", "delete", "cluster", "--name", moveTargetCluster)
	return nil
}

func ensureMoveTarget() error {
	clusters, err := sh.Output("kind", "get", "clusters")
	if err != nil {
		return err
	}
	if !slices.Contains(strings.Fields(clusters), moveTargetCluster) {
		configPath := filepath.Join(workDir, "kind-target.yaml")
		// The same config as the source: CAPD on the target has to create sibling containers
		// too, and after the pivot it is the one managing the machines that already exist.
		if err := os.WriteFile(configPath, []byte(kindConfig()), 0o644); err != nil { //nolint:gosec // config file
			return err
		}
		if err := sh.RunV("kind", "create", "cluster", "--name", moveTargetCluster,
			"--config", configPath); err != nil {
			return err
		}
	}
	kubeconfig, err := sh.Output("kind", "get", "kubeconfig", "--name", moveTargetCluster)
	if err != nil {
		return err
	}
	// Both management clusters and the registry share one docker network, so the moved
	// machines stay reachable by the same names from the target.
	if err := sh.RunV("docker", "network", "connect", "kind", registryName); err != nil {
		connected, inspectErr := sh.Output("docker", "network", "inspect", "kind",
			"-f", "{{range .Containers}}{{.Name}} {{end}}")
		if inspectErr != nil || !strings.Contains(connected, registryName) {
			return err
		}
	}
	return os.WriteFile(moveTargetKubeconfig(), []byte(kubeconfig), 0o600)
}

func configureTargetRegistry() error {
	nodes, err := sh.Output("kind", "get", "nodes", "--name", moveTargetCluster)
	if err != nil {
		return err
	}
	return writeHostsToml(strings.Fields(nodes))
}

func clusterctlInitTarget() error {
	if err := targetKubectl("get", "namespace", "capd-system"); err == nil {
		return nil
	}
	// The provider versions have to match the source's, or the move refuses: clusterctl
	// compares what is installed on both sides before it touches anything.
	return sh.RunWithV(map[string]string{"CLUSTER_TOPOLOGY": "true"},
		clusterctlPath(), "init", "--infrastructure", "docker:v"+clusterAPIVersion,
		"--kubeconfig", moveTargetKubeconfig())
}

func waitTargetProviders() error {
	for _, ns := range []string{
		"capi-system",
		"capi-kubeadm-bootstrap-system",
		"capi-kubeadm-control-plane-system",
		"capd-system",
	} {
		if err := targetKubectl("wait", "--for=condition=Available", "deployment", "--all",
			"-n", ns, "--timeout=5m"); err != nil {
			return err
		}
	}
	return nil
}

// loadControllerImageInto loads the already-built controller image into the target. e2e:run
// built it; this target does not rebuild, so a pivot test always runs the same binary the
// source cluster is running.
func loadControllerImageInto() error {
	return sh.RunV("kind", "load", "docker-image", controllerImage, "--name", moveTargetCluster)
}

func installControllerOnTarget() error {
	manifestPath := filepath.Join(workDir, "controller.yaml")
	if _, err := os.Stat(manifestPath); err != nil {
		return fmt.Errorf("no rendered controller manifest at %s - run e2e:run first", manifestPath)
	}
	if err := targetKubectl("apply", "-f", manifestPath); err != nil {
		return err
	}
	return targetKubectl("-n", "caapz-system", "rollout", "status",
		"deployment/caapz-controller-manager", "--timeout=3m")
}

func targetKubectl(args ...string) error {
	return sh.RunV("kubectl", append([]string{"--kubeconfig", moveTargetKubeconfig()}, args...)...)
}
