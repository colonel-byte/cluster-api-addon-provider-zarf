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

// Package e2e holds the end-to-end suite from dist/e2e-test-plan.md. It is a separate Go module
// on purpose: the root module's build is vendored and offline, and nothing here may land in the
// root vendor/. It runs only through `go run ./magefiles/core e2e:run` against a topology
// `e2e:up` built; without E2E_KUBECONFIG every test skips.
package e2e

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	workloadCluster = "e2e-workload"
	// registryHost must match magefiles/pkg/e2e - it is the one stable repository name.
	registryHost = "caapz-e2e-registry:5000"
	spikeImage   = registryHost + "/spike/busybox:test"
)

// clientset returns a client for the management cluster, or skips the test when the suite is
// not being driven by e2e:run.
func clientset(t *testing.T) *kubernetes.Clientset {
	t.Helper()
	kubeconfig := os.Getenv("E2E_KUBECONFIG")
	if kubeconfig == "" {
		t.Skip("E2E_KUBECONFIG is not set; run this suite with `go run ./magefiles/core e2e:run`")
	}
	cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		t.Fatalf("unable to load the management kubeconfig: %v", err)
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatalf("unable to build a clientset: %v", err)
	}
	return cs
}

// workloadServer reads the CAPD-written kubeconfig secret - the exact artifact a deploy Job will
// mount - and returns the host and port of the API server address as written in it.
func workloadServer(t *testing.T, cs *kubernetes.Clientset) (string, string) {
	t.Helper()
	secret, err := cs.CoreV1().Secrets("default").Get(t.Context(), workloadCluster+"-kubeconfig", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("unable to read the %s-kubeconfig secret: %v", workloadCluster, err)
	}
	kubeconfig, err := clientcmd.Load(secret.Data["value"])
	if err != nil {
		t.Fatalf("unable to parse the kubeconfig in the secret: %v", err)
	}
	for _, cluster := range kubeconfig.Clusters {
		server, err := url.Parse(cluster.Server)
		if err != nil {
			t.Fatalf("unable to parse the server address %q: %v", cluster.Server, err)
		}
		port := server.Port()
		if port == "" {
			port = "443"
		}
		return server.Hostname(), port
	}
	t.Fatal("the kubeconfig secret names no cluster")
	return "", ""
}

// TestSpikeKubeconfigReachableFromPod is Phase 0 spike assertion #1, the go/no-go the deploy-Job
// model rests on: the server address written in the CAPD kubeconfig secret must be reachable
// from a pod in the management cluster. The pod image is pulled from the local registry by its
// stable name, which is spike assertion #2's management-cluster half in the same breath.
func TestSpikeKubeconfigReachableFromPod(t *testing.T) {
	cs := clientset(t)
	host, port := workloadServer(t, cs)
	t.Logf("workload API server as written in the secret: %s:%s", host, port)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "spike-reach", Namespace: "default"},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Containers: []corev1.Container{{
				Name:    "nc",
				Image:   spikeImage,
				Command: []string{"nc", "-z", "-w", "15", host, port},
			}},
		},
	}
	_ = cs.CoreV1().Pods("default").Delete(t.Context(), pod.Name, metav1.DeleteOptions{})
	waitGone(t, cs, pod.Name)
	if _, err := cs.CoreV1().Pods("default").Create(t.Context(), pod, metav1.CreateOptions{}); err != nil {
		t.Fatalf("unable to create the spike pod: %v", err)
	}
	t.Cleanup(func() {
		_ = cs.CoreV1().Pods("default").Delete(context.Background(), pod.Name, metav1.DeleteOptions{})
	})

	if err := waitPhase(t, cs, pod.Name, corev1.PodSucceeded); err != nil {
		t.Fatalf("the workload API server %s:%s is not reachable from a management pod "+
			"(or the registry pull failed - check the pod events): %v", host, port, err)
	}
}

// TestSpikeRegistryPullFromWorkloadNode is spike assertion #2's workload half: a workload-cluster
// node resolves and pulls from the registry by the same repository string a proxy would carry.
// It drives crictl over docker exec because the CAPD machines are host-visible containers.
func TestSpikeRegistryPullFromWorkloadNode(t *testing.T) {
	clientset(t) // skip-gate only; the assertion runs over the docker CLI

	out, err := exec.Command("docker", "ps", "-q",
		"--filter", "label=io.x-k8s.kind.cluster="+workloadCluster).Output()
	if err != nil {
		t.Fatalf("unable to list the workload machine containers: %v", err)
	}
	nodes := strings.Fields(string(out))
	if len(nodes) == 0 {
		t.Fatalf("no running containers labeled io.x-k8s.kind.cluster=%s", workloadCluster)
	}

	pull := exec.Command("docker", "exec", nodes[0], "crictl", "pull", spikeImage)
	if combined, err := pull.CombinedOutput(); err != nil {
		t.Fatalf("crictl pull %s on a workload node failed: %v\n%s", spikeImage, err, combined)
	}
}

func waitPhase(t *testing.T, cs *kubernetes.Clientset, name string, phase corev1.PodPhase) error {
	t.Helper()
	return wait.PollUntilContextTimeout(t.Context(), 3*time.Second, 3*time.Minute, true,
		func(ctx context.Context) (bool, error) {
			pod, err := cs.CoreV1().Pods("default").Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				return false, err
			}
			if pod.Status.Phase == corev1.PodFailed && phase != corev1.PodFailed {
				return false, fmt.Errorf("pod %s failed: %+v", name, pod.Status.ContainerStatuses)
			}
			return pod.Status.Phase == phase, nil
		})
}

func waitGone(t *testing.T, cs *kubernetes.Clientset, name string) {
	t.Helper()
	_ = wait.PollUntilContextTimeout(t.Context(), time.Second, time.Minute, true,
		func(ctx context.Context) (bool, error) {
			_, err := cs.CoreV1().Pods("default").Get(ctx, name, metav1.GetOptions{})
			return err != nil, nil
		})
}
