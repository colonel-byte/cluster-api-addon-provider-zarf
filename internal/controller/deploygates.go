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
	"fmt"

	batchv1 "k8s.io/api/batch/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	addonsv1alpha1 "github.com/colonel-byte/cluster-api-addon-provider-zarf/api/v1alpha1"
)

// convergeDeploys walks each matched cluster's release through §6 steps 3, 5, and 6: the
// ApplyOnce seed check, the init-first eligibility gate, the one-Job-per-cluster rule, and
// finally the deploy Job itself. Finished Jobs are the status-plumbing pass's concern; this one
// only decides whether a Job may start.
func (r *ZarfPackageProxyReconciler) convergeDeploys(ctx context.Context, proxy *addonsv1alpha1.ZarfPackageProxy, matched []clusterv1.Cluster) error {
	log := ctrl.LoggerFrom(ctx)

	for i := range matched {
		cluster := &matched[i]
		release, err := r.findRelease(ctx, client.ObjectKeyFromObject(proxy), cluster)
		if err != nil {
			return err
		}
		if release == nil {
			continue
		}

		before := release.DeepCopy()
		stamped, err := r.convergeDeploy(ctx, proxy, cluster, release)
		if err != nil {
			// A workload-cluster probe failing is §7B degradation, not a reconcile failure:
			// log, leave the release as it stands, and let the next pass retry.
			log.Info("deploy gating skipped this cluster", "cluster", cluster.Name, "error", err.Error())
			continue
		}
		if stamped != nil {
			if _, err := r.EnsureDeployJob(ctx, stamped); err != nil {
				return err
			}
		}
		if err := r.Status().Patch(ctx, release, client.MergeFrom(before)); err != nil {
			return err
		}
	}
	return nil
}

// convergeDeploy applies the gates to one release, mutating its status in place and returning
// the Job to stamp when every gate passes. A nil Job with nil error means the release holds.
func (r *ZarfPackageProxyReconciler) convergeDeploy(ctx context.Context, proxy *addonsv1alpha1.ZarfPackageProxy, cluster *clusterv1.Cluster, release *addonsv1alpha1.ZarfPackageRelease) (*batchv1.Job, error) {
	setReady := func(status metav1.ConditionStatus, reason, message string) {
		meta.SetStatusCondition(&release.Status.Conditions, metav1.Condition{
			Type:    addonsv1alpha1.ConditionReady,
			Status:  status,
			Reason:  reason,
			Message: message,
		})
	}

	// §3's first tier: an applied ApplyOnce release never round-trips anywhere again.
	if proxy.Spec.Strategy == addonsv1alpha1.StrategyApplyOnce && release.Status.Applied {
		release.Status.Phase = addonsv1alpha1.PhaseCompleted
		return nil, nil
	}

	// The deploy spec is the Job's entire input; without it there is nothing to stamp.
	if !meta.IsStatusConditionTrue(release.Status.Conditions, addonsv1alpha1.ConditionDeploySpecReady) {
		return nil, nil
	}
	// §5B: a dependency verdict other than satisfied holds the release in Pending; the
	// DependenciesSatisfied condition already names why.
	if dep := meta.FindStatusCondition(release.Status.Conditions, addonsv1alpha1.ConditionDependenciesSatisfied); dep != nil && dep.Status != metav1.ConditionTrue {
		release.Status.Phase = addonsv1alpha1.PhasePending
		return nil, nil
	}

	// An existing Job blocks a new one: running means Deploying, finished means its outcome is
	// absorbed into status - and a stale finished Job is deleted there, so the next pass
	// stamps the redeploy.
	existing := &batchv1.Job{}
	err := r.Get(ctx, client.ObjectKey{Namespace: release.Namespace, Name: release.Name + "-deploy"}, existing)
	switch {
	case err == nil:
		if DeployJobPhase(existing, proxy.Spec.Strategy) == addonsv1alpha1.PhaseDeploying {
			release.Status.Phase = addonsv1alpha1.PhaseDeploying
			setReady(metav1.ConditionFalse, addonsv1alpha1.ReasonDeploying, "the deploy Job is running")
			return nil, nil
		}
		return nil, r.absorbFinishedJob(ctx, proxy, release, existing)
	case !apierrors.IsNotFound(err):
		return nil, err
	}

	// From here every gate may need the workload cluster; build the client once.
	workload, err := r.workloadClient(ctx, release.Namespace, cluster.Name)
	if err != nil {
		return nil, err
	}

	// §3's second tier, before any deploy would fire: adopt an existing deployed-package
	// record instead of re-executing the seed.
	if proxy.Spec.Strategy == addonsv1alpha1.StrategyApplyOnce {
		if proxy.Status.PackageName == "" {
			// shortcut: a proxy resolved before packageName was recorded cannot be probed;
			// hold rather than risk re-running a seed. Recreating the proxy re-resolves.
			return nil, fmt.Errorf("the proxy's status carries no packageName to probe for")
		}
		adopted, err := hasDeployedPackage(ctx, workload, proxy.Status.PackageName)
		if err != nil {
			return nil, err
		}
		if adopted {
			release.Status.Phase = addonsv1alpha1.PhaseCompleted
			release.Status.Applied = true
			setReady(metav1.ConditionTrue, addonsv1alpha1.ReasonAdoptedExistingRecord,
				fmt.Sprintf("the cluster already carries zarf's record for package %q; adopted without re-executing", proxy.Status.PackageName))
			return nil, nil
		}
	}

	// §6 step 5: on a cluster with no zarf state, only init-capable releases may take the Job
	// slot. No carve-out for stateless packages: v1alpha1's metadata.yolo is deprecated with
	// no v1beta1 successor, so "needs no init" becomes a deploy-time connectivity knob on the
	// proxy when one is designed - never package metadata.
	initCapable := proxy.Status.PackageKind == "ZarfInitConfig" ||
		(proxy.Spec.Init != nil && proxy.Spec.Init.AutoInit)
	if !initCapable {
		hasState, err := hasZarfState(ctx, workload)
		if err != nil {
			return nil, err
		}
		if !hasState {
			release.Status.Phase = addonsv1alpha1.PhasePending
			setReady(metav1.ConditionFalse, addonsv1alpha1.ReasonWaitingForZarfState,
				"the cluster has no zarf state; only an init-capable release may deploy first")
			return nil, nil
		}
	}

	// §6 step 6: one deploy or removal Job per cluster at a time, unless the Cluster opts out.
	if cluster.Annotations[addonsv1alpha1.AnnotationConcurrentDeploys] != "true" {
		jobs := &batchv1.JobList{}
		if err := r.List(ctx, jobs, client.InNamespace(release.Namespace),
			client.MatchingLabels{clusterLabel: cluster.Name}); err != nil {
			return nil, err
		}
		for j := range jobs.Items {
			if DeployJobPhase(&jobs.Items[j], addonsv1alpha1.StrategyReconcile) == addonsv1alpha1.PhaseDeploying {
				release.Status.Phase = addonsv1alpha1.PhasePending
				setReady(metav1.ConditionFalse, addonsv1alpha1.ReasonWaitingForActiveDeploy,
					fmt.Sprintf("Job %s holds the cluster's deploy slot", jobs.Items[j].Name))
				return nil, nil
			}
		}
	}

	release.Status.Phase = addonsv1alpha1.PhaseDeploying
	setReady(metav1.ConditionFalse, addonsv1alpha1.ReasonDeploying, "the deploy Job is starting")
	job := BuildDeployJob(proxy, release, r.DeployImage, r.CachePath)
	// The spec hash is what later marks this Job stale once an edit re-renders the spec.
	if hash := r.deploySpecHash(ctx, release); hash != "" {
		job.Annotations = map[string]string{specHashAnnotation: hash}
	}
	return job, nil
}
