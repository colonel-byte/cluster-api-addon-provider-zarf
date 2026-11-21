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

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	addonsv1alpha1 "github.com/colonel-byte/cluster-api-addon-provider-zarf/api/v1alpha1"
)

// Each deploy runs in its own Kubernetes Job rather than in the controller process, because the
// Zarf SDK resolves its target cluster from the process environment (KUBECONFIG) and because
// package actions execute arbitrary commands on the deploying machine. A Job gives every deploy
// its own environment, its own blast radius, and a lifetime independent of the controller pod.

const (
	// cacheVolumeName is the deploy pod's package cache volume. A proxy's deployJob.volumes
	// entry with this name replaces the default emptyDir.
	cacheVolumeName = "zarf-cache"

	// kubeconfigMountPath is where the target cluster's kubeconfig secret mounts; KUBECONFIG
	// points at its "value" key, the key CAPI writes.
	kubeconfigMountPath = "/etc/caapz/kubeconfig"

	// specMountPath is where the rendered deploy spec mounts. The deploy pod acts as a pure
	// operator: its entire input is files on disk - this spec, the kubeconfig, the cache -
	// and it holds no management-cluster credentials or API access. Results return in the
	// pod's termination message; the controller patches release status.
	specMountPath = "/etc/caapz/spec"

	// releaseLabel on a deploy Job names the ZarfPackageRelease it realizes.
	releaseLabel = "addons.colonel-byte.dev/release"
)

// BuildDeployJob renders the Job that deploys proxy's package for release. defaultImage and
// cachePath come from the manager's --deploy-image and --cache-path flags; the proxy's deployJob
// knobs override pod-level details.
func BuildDeployJob(proxy *addonsv1alpha1.ZarfPackageProxy, release *addonsv1alpha1.ZarfPackageRelease, defaultImage, cachePath string) *batchv1.Job {
	opts := proxy.Spec.DeployJob
	if opts == nil {
		opts = &addonsv1alpha1.DeployJobOptions{}
	}

	image := defaultImage
	if opts.Image != "" {
		image = opts.Image
	}

	podSecurity := opts.PodSecurityContext
	if podSecurity == nil {
		podSecurity = &corev1.PodSecurityContext{
			RunAsNonRoot:   ptr.To(true),
			SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
		}
	}
	containerSecurity := opts.SecurityContext
	if containerSecurity == nil {
		containerSecurity = &corev1.SecurityContext{
			AllowPrivilegeEscalation: ptr.To(false),
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
			ReadOnlyRootFilesystem:   ptr.To(true),
		}
	}

	volumes := []corev1.Volume{
		{
			Name: "kubeconfig",
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					// The admin kubeconfig secret CAPI maintains per cluster.
					SecretName: release.Spec.ClusterRef.Name + "-kubeconfig",
				},
			},
		},
		{
			Name: "spec",
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					// shortcut: the controller does not write this Secret yet; it lands
					// with the resolver - the rendered deploy spec (package repository
					// pinned by digest, rendered values and variables, component
					// selection), stamped before the Job and owned by the release.
					SecretName: release.Name + "-deploy-spec",
				},
			},
		},
	}
	cacheProvided := false
	for _, v := range opts.Volumes {
		if v.Name == cacheVolumeName {
			cacheProvided = true
		}
		volumes = append(volumes, v)
	}
	if !cacheProvided {
		volumes = append(volumes, corev1.Volume{
			Name:         cacheVolumeName,
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
		})
	}

	mounts := append([]corev1.VolumeMount{
		{Name: "kubeconfig", MountPath: kubeconfigMountPath, ReadOnly: true},
		{Name: "spec", MountPath: specMountPath, ReadOnly: true},
		{Name: cacheVolumeName, MountPath: cachePath},
	}, opts.VolumeMounts...)

	// shortcut: the "deploy" subcommand these args target does not exist yet; it lands with
	// the zarf SDK integration. It reads everything from the mounted spec file - never the
	// management cluster - and reports through the pod termination message.
	args := []string{
		"deploy",
		"--spec", specMountPath + "/spec.yaml",
		"--cache-path", cachePath,
	}

	// shortcut: the Job name is release.Name + "-deploy" with no truncation; releases named
	// past 56 characters break it. Hash-truncate when real cluster names arrive.
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      release.Name + "-deploy",
			Namespace: release.Namespace,
			Labels: map[string]string{
				releaseLabel: release.Name,
			},
			// Owned by the release so the Job is garbage-collected with it and release
			// events requeue on Job completion once the watch is wired.
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion:         addonsv1alpha1.GroupVersion.String(),
				Kind:               "ZarfPackageRelease",
				Name:               release.Name,
				UID:                release.UID,
				Controller:         ptr.To(true),
				BlockOwnerDeletion: ptr.To(true),
			}},
		},
		Spec: batchv1.JobSpec{
			// The reconciler owns retries, so the release stays the single record of
			// attempts; a failed pod fails the Job immediately.
			BackoffLimit: ptr.To(int32(0)),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{releaseLabel: release.Name},
				},
				Spec: corev1.PodSpec{
					RestartPolicy:      corev1.RestartPolicyNever,
					ServiceAccountName: opts.ServiceAccountName,
					SecurityContext:    podSecurity,
					NodeSelector:       opts.NodeSelector,
					Tolerations:        opts.Tolerations,
					Volumes:            volumes,
					Containers: []corev1.Container{{
						Name:            "deploy",
						Image:           image,
						Args:            args,
						SecurityContext: containerSecurity,
						VolumeMounts:    mounts,
						Env: []corev1.EnvVar{{
							Name:  "KUBECONFIG",
							Value: kubeconfigMountPath + "/value",
						}},
					}},
				},
			},
		},
	}
}

// EnsureDeployJob returns the release's deploy Job, creating job when none exists. An existing
// Job is returned as found - never mutated, so an in-flight deploy is not disturbed.
func (r *ZarfPackageProxyReconciler) EnsureDeployJob(ctx context.Context, job *batchv1.Job) (*batchv1.Job, error) {
	existing := &batchv1.Job{}
	err := r.Get(ctx, client.ObjectKeyFromObject(job), existing)
	if err == nil {
		return existing, nil
	}
	if !apierrors.IsNotFound(err) {
		return nil, err
	}
	return job, r.Create(ctx, job)
}

// DeployJobPhase maps a deploy Job's state to the release phase it implies: Deploying while the
// Job runs, Failed on the Failed condition, and Deployed - or Completed, for ApplyOnce - on the
// Complete condition.
func DeployJobPhase(job *batchv1.Job, strategy addonsv1alpha1.DeploymentStrategy) addonsv1alpha1.ReleasePhase {
	for _, c := range job.Status.Conditions {
		if c.Status != corev1.ConditionTrue {
			continue
		}
		switch c.Type {
		case batchv1.JobComplete:
			if strategy == addonsv1alpha1.StrategyApplyOnce {
				return addonsv1alpha1.PhaseCompleted
			}
			return addonsv1alpha1.PhaseDeployed
		case batchv1.JobFailed:
			return addonsv1alpha1.PhaseFailed
		}
	}
	return addonsv1alpha1.PhaseDeploying
}
