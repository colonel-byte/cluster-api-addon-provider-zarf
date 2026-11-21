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
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	addonsv1alpha1 "github.com/colonel-byte/cluster-api-addon-provider-zarf/api/v1alpha1"
)

func testRelease() *addonsv1alpha1.ZarfPackageRelease {
	return &addonsv1alpha1.ZarfPackageRelease{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "example-security-baseline",
			Namespace: "fleet-default",
		},
		Spec: addonsv1alpha1.ZarfPackageReleaseSpec{
			ClusterRef:      corev1.LocalObjectReference{Name: "example-cluster"},
			PackageProxyRef: addonsv1alpha1.NamespacedReference{Name: "security-baseline"},
		},
	}
}

func testProxy(opts *addonsv1alpha1.DeployJobOptions) *addonsv1alpha1.ZarfPackageProxy {
	return &addonsv1alpha1.ZarfPackageProxy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "security-baseline",
			Namespace: "fleet-default",
		},
		Spec: addonsv1alpha1.ZarfPackageProxySpec{
			Package: addonsv1alpha1.ZarfPackageSource{
				Repository: "registry.local/packages/security-baseline",
				Version:    "1.4.2",
			},
			DeployJob: opts,
		},
	}
}

func TestBuildDeployJobDefaults(t *testing.T) {
	job := BuildDeployJob(testProxy(nil), testRelease(), "controller:dev", "/var/lib/caapz/cache")

	if job.Name != "example-security-baseline-deploy" || job.Namespace != "fleet-default" {
		t.Errorf("unexpected name/namespace: %s/%s", job.Namespace, job.Name)
	}
	owner := job.OwnerReferences[0]
	if owner.Kind != "ZarfPackageRelease" || owner.Controller == nil || !*owner.Controller {
		t.Errorf("job is not controller-owned by the release: %+v", owner)
	}
	if *job.Spec.BackoffLimit != 0 {
		t.Errorf("backoffLimit = %d, want 0 (the reconciler owns retries)", *job.Spec.BackoffLimit)
	}

	pod := job.Spec.Template.Spec
	container := pod.Containers[0]
	if container.Image != "controller:dev" {
		t.Errorf("image = %q, want the --deploy-image default", container.Image)
	}
	if container.Env[0].Name != "KUBECONFIG" || container.Env[0].Value != "/etc/caapz/kubeconfig/value" {
		t.Errorf("KUBECONFIG env wrong: %+v", container.Env)
	}
	if pod.SecurityContext.RunAsNonRoot == nil || !*pod.SecurityContext.RunAsNonRoot {
		t.Error("default pod security context is not runAsNonRoot")
	}

	byName := map[string]corev1.Volume{}
	for _, v := range pod.Volumes {
		byName[v.Name] = v
	}
	if byName["kubeconfig"].Secret == nil || byName["kubeconfig"].Secret.SecretName != "example-cluster-kubeconfig" {
		t.Errorf("kubeconfig volume wrong: %+v", byName["kubeconfig"])
	}
	if byName[cacheVolumeName].EmptyDir == nil {
		t.Errorf("default cache volume is not an emptyDir: %+v", byName[cacheVolumeName])
	}
	if byName["spec"].Secret == nil || byName["spec"].Secret.SecretName != "example-security-baseline-deploy-spec" {
		t.Errorf("rendered-spec volume wrong: %+v", byName["spec"])
	}
	for _, arg := range container.Args {
		if strings.Contains(arg, ":") && strings.Contains(arg, "/") {
			t.Errorf("args carry a registry reference (%q); the pod's only inputs are mounted files", arg)
		}
	}
}

func TestBuildDeployJobKnobs(t *testing.T) {
	job := BuildDeployJob(testProxy(&addonsv1alpha1.DeployJobOptions{
		Image:              "custom:1",
		ServiceAccountName: "deployer",
		PodSecurityContext: &corev1.PodSecurityContext{FSGroup: ptr.To(int64(2000))},
		Volumes: []corev1.Volume{
			{
				Name: cacheVolumeName,
				VolumeSource: corev1.VolumeSource{
					PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "big-cache"},
				},
			},
		},
		NodeSelector: map[string]string{"disk": "large"},
	}), testRelease(), "controller:dev", "/var/lib/caapz/cache")

	pod := job.Spec.Template.Spec
	if pod.Containers[0].Image != "custom:1" {
		t.Errorf("image = %q, want the proxy override", pod.Containers[0].Image)
	}
	if pod.ServiceAccountName != "deployer" || pod.NodeSelector["disk"] != "large" {
		t.Errorf("serviceAccount/nodeSelector knobs not applied: %+v", pod)
	}
	if pod.SecurityContext.FSGroup == nil || *pod.SecurityContext.FSGroup != 2000 {
		t.Errorf("podSecurityContext knob not applied: %+v", pod.SecurityContext)
	}
	cacheCount := 0
	for _, v := range pod.Volumes {
		if v.Name == cacheVolumeName {
			cacheCount++
			if v.PersistentVolumeClaim == nil {
				t.Errorf("user cache volume not used: %+v", v)
			}
		}
	}
	if cacheCount != 1 {
		t.Errorf("cache volume count = %d, want exactly 1", cacheCount)
	}
}

func TestDeployJobPhase(t *testing.T) {
	complete := batchv1.JobStatus{
		Conditions: []batchv1.JobCondition{
			{
				Type:   batchv1.JobComplete,
				Status: corev1.ConditionTrue,
			},
		},
	}
	failed := batchv1.JobStatus{
		Conditions: []batchv1.JobCondition{
			{
				Type:   batchv1.JobFailed,
				Status: corev1.ConditionTrue,
			},
		},
	}

	tests := []struct {
		name     string
		status   batchv1.JobStatus
		strategy addonsv1alpha1.DeploymentStrategy
		want     addonsv1alpha1.ReleasePhase
	}{
		{
			name:     "running job",
			status:   batchv1.JobStatus{},
			strategy: addonsv1alpha1.StrategyReconcile,
			want:     addonsv1alpha1.PhaseDeploying,
		},
		{
			name:     "complete reconcile job",
			status:   complete,
			strategy: addonsv1alpha1.StrategyReconcile,
			want:     addonsv1alpha1.PhaseDeployed,
		},
		{
			name:     "complete apply-once job",
			status:   complete,
			strategy: addonsv1alpha1.StrategyApplyOnce,
			want:     addonsv1alpha1.PhaseCompleted,
		},
		{
			name:     "failed job",
			status:   failed,
			strategy: addonsv1alpha1.StrategyReconcile,
			want:     addonsv1alpha1.PhaseFailed,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := DeployJobPhase(&batchv1.Job{Status: tt.status}, tt.strategy)
			if got != tt.want {
				t.Errorf("DeployJobPhase() = %q, want %q", got, tt.want)
			}
		})
	}
}
