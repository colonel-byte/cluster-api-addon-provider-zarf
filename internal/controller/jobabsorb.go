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
	"crypto/sha256"
	"encoding/hex"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	addonsv1alpha1 "github.com/colonel-byte/cluster-api-addon-provider-zarf/api/v1alpha1"
	"github.com/colonel-byte/cluster-api-addon-provider-zarf/internal/deploy"
	"github.com/colonel-byte/cluster-api-addon-provider-zarf/internal/deployspec"
)

// specHashAnnotation on a deploy Job records the hash of the deploy spec it was stamped from.
// A finished Job whose annotation no longer matches the current rendering is stale: its work
// predates an edit, and under strategy Reconcile it is deleted so the next pass redeploys.
// Delete-and-recreate rather than generation-suffixed names, so the one-Job-per-cluster listing
// and the <release>-deploy lookup stay single-valued.
const specHashAnnotation = "addons.colonel-byte.dev/spec-hash"

// deploySpecHash hashes the release's current rendered deploy spec; empty when the Secret is
// not readable, which disables staleness detection rather than failing the pass.
func (r *ZarfPackageProxyReconciler) deploySpecHash(ctx context.Context, release *addonsv1alpha1.ZarfPackageRelease) string {
	secret := &corev1.Secret{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: release.Namespace, Name: release.Name + "-deploy-spec"}, secret); err != nil {
		return ""
	}
	sum := sha256.Sum256(secret.Data[deployspec.FileName])
	return hex.EncodeToString(sum[:])
}

// absorbFinishedJob reads a finished deploy Job's outcome into the release - §6 step 6's
// "results return in the pod's termination message" is read here - and deletes the Job when its
// rendering has gone stale under strategy Reconcile, so the next pass stamps a fresh deploy.
// Idempotent: re-absorbing a finished Job writes the same status again.
func (r *ZarfPackageProxyReconciler) absorbFinishedJob(ctx context.Context, proxy *addonsv1alpha1.ZarfPackageProxy, release *addonsv1alpha1.ZarfPackageRelease, job *batchv1.Job) error {
	log := ctrl.LoggerFrom(ctx)
	result, found := r.jobResult(ctx, job)

	if DeployJobPhase(job, proxy.Spec.Strategy) == addonsv1alpha1.PhaseFailed {
		release.Status.Phase = addonsv1alpha1.PhaseFailed
		message := result.Error
		if message == "" {
			message = "the deploy Job failed and its pod left no termination message"
		}
		meta.SetStatusCondition(&release.Status.Conditions, metav1.Condition{
			Type:    addonsv1alpha1.ConditionReady,
			Status:  metav1.ConditionFalse,
			Reason:  addonsv1alpha1.ReasonDeployFailed,
			Message: message,
		})
	} else {
		reason := addonsv1alpha1.ReasonDeployed
		release.Status.Phase = addonsv1alpha1.PhaseDeployed
		if proxy.Spec.Strategy == addonsv1alpha1.StrategyApplyOnce {
			release.Status.Phase = addonsv1alpha1.PhaseCompleted
			release.Status.Applied = true
			reason = addonsv1alpha1.ReasonCompleted
			if err := r.checkSeedRecord(ctx, proxy, release); err != nil {
				log.Info("unable to check the seed record after completion", "error", err.Error())
			}
		}
		if found {
			if i := strings.LastIndex(result.Package, "@"); i >= 0 {
				release.Status.DeployedDigest = result.Package[i+1:]
			}
			release.Status.DeployedVersion = result.Version
		} else {
			log.Info("the completed deploy pod left no readable result; deployedDigest stays unset",
				"job", job.Name)
		}
		message := "the deploy Job completed"
		if result.Package != "" {
			message = "package " + result.Package + " is on the cluster"
		}
		meta.SetStatusCondition(&release.Status.Conditions, metav1.Condition{
			Type:    addonsv1alpha1.ConditionReady,
			Status:  metav1.ConditionTrue,
			Reason:  reason,
			Message: message,
		})
	}

	// Staleness: an edit re-rendered the spec after this Job was stamped. Reconcile releases
	// redeploy - a failed Job also re-runs on an edit, which is the retry story for now.
	// ApplyOnce never re-executes, so its finished Job stays as the record of the run.
	if proxy.Spec.Strategy == addonsv1alpha1.StrategyApplyOnce {
		return nil
	}
	if hash := r.deploySpecHash(ctx, release); hash != "" && job.Annotations[specHashAnnotation] != hash {
		log.Info("the deploy spec changed since this Job ran; deleting it to redeploy", "job", job.Name)
		propagation := metav1.DeletePropagationBackground
		if err := r.Delete(ctx, job, &client.DeleteOptions{PropagationPolicy: &propagation}); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

// checkSeedRecord surfaces §3's degradation: a completed seed deploy that left no
// deployed-package record on the cluster - a pure-actions package - gets SeedRecorded: False.
func (r *ZarfPackageProxyReconciler) checkSeedRecord(ctx context.Context, proxy *addonsv1alpha1.ZarfPackageProxy, release *addonsv1alpha1.ZarfPackageRelease) error {
	if proxy.Status.PackageName == "" {
		return nil
	}
	workload, err := r.workloadClient(ctx, release.Namespace, release.Spec.ClusterRef.Name)
	if err != nil {
		return err
	}
	recorded, err := hasDeployedPackage(ctx, workload, proxy.Status.PackageName)
	if err != nil {
		return err
	}
	if !recorded {
		meta.SetStatusCondition(&release.Status.Conditions, metav1.Condition{
			Type:   addonsv1alpha1.ConditionSeedRecorded,
			Status: metav1.ConditionFalse,
			Reason: addonsv1alpha1.ReasonNoDeployedPackageRecord,
			Message: "the completed seed deploy left no deployed-package record on the cluster; " +
				"the never-re-execute guarantee rests on this release object alone",
		})
	}
	return nil
}

// jobResult reads the deploy pod's termination message back as the result document. False when
// no pod carries a parseable message - an evicted or hand-deleted pod leaves none.
func (r *ZarfPackageProxyReconciler) jobResult(ctx context.Context, job *batchv1.Job) (deploy.Result, bool) {
	pods := &corev1.PodList{}
	if err := r.List(ctx, pods, client.InNamespace(job.Namespace),
		client.MatchingLabels{batchv1.JobNameLabel: job.Name}); err != nil {
		return deploy.Result{}, false
	}
	for i := range pods.Items {
		for _, status := range pods.Items[i].Status.ContainerStatuses {
			terminated := status.State.Terminated
			if terminated == nil || terminated.Message == "" {
				continue
			}
			var result deploy.Result
			if err := yaml.Unmarshal([]byte(terminated.Message), &result); err != nil {
				continue
			}
			return result, true
		}
	}
	return deploy.Result{}, false
}
