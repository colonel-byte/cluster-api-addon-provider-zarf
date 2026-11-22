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
	"time"

	batchv1 "k8s.io/api/batch/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	addonsv1alpha1 "github.com/colonel-byte/cluster-api-addon-provider-zarf/api/v1alpha1"
)

// removalRetryDelay paces re-running a failed removal Job.
// shortcut: a fixed interval rather than exponential backoff; grow it if a hopeless removal
// hammering a cluster every two minutes ever matters in practice.
const removalRetryDelay = 2 * time.Minute

// removalOwed says whether proxy's releases owe an uninstall on deletion (§8A): policy Delete -
// the default - except under ApplyOnce, which always behaves as Retain because a seed cannot be
// un-run.
func removalOwed(proxy *addonsv1alpha1.ZarfPackageProxy) bool {
	return proxy.Spec.DeletionPolicy != addonsv1alpha1.PolicyRetain &&
		proxy.Spec.Strategy != addonsv1alpha1.StrategyApplyOnce
}

// finalizersFor is the finalizer list a newly stamped release starts with.
func finalizersFor(proxy *addonsv1alpha1.ZarfPackageProxy) []string {
	if removalOwed(proxy) {
		return []string{addonsv1alpha1.FinalizerRemoval}
	}
	return nil
}

// finalizeReleases is §8B's finalizer machinery over every release realizing proxy. Live
// releases carry the removal finalizer exactly while a removal is owed - tracking the proxy's
// current policy, so a flip to Retain is the declarative break-glass for a stuck removal.
// Deleting releases run the removal Job and drop the finalizer on success; §7 outranks all of
// it, so a deleting, missing, or unreachable cluster drops the finalizer without removal. The
// returned duration, when non-zero, asks for a requeue - the failed-removal retry timer.
func (r *ZarfPackageProxyReconciler) finalizeReleases(ctx context.Context, proxy *addonsv1alpha1.ZarfPackageProxy) (time.Duration, error) {
	releases, err := r.listProxyReleases(ctx, proxy)
	if err != nil {
		return 0, err
	}
	owed := removalOwed(proxy)

	var requeue time.Duration
	for _, release := range releases {
		if release.DeletionTimestamp.IsZero() {
			changed := controllerutil.RemoveFinalizer(release, addonsv1alpha1.FinalizerRemoval)
			if owed {
				changed = controllerutil.AddFinalizer(release, addonsv1alpha1.FinalizerRemoval)
			}
			if changed {
				if err := r.Update(ctx, release); err != nil {
					return 0, err
				}
			}
			continue
		}

		if !controllerutil.ContainsFinalizer(release, addonsv1alpha1.FinalizerRemoval) {
			continue
		}
		if !owed {
			// The policy flipped to Retain while this deletion waited: orphan, as declared.
			if err := r.dropRemovalFinalizer(ctx, release); err != nil {
				return 0, err
			}
			continue
		}
		after, err := r.finalizeRelease(ctx, proxy, release)
		if err != nil {
			return 0, err
		}
		if after > 0 && (requeue == 0 || after < requeue) {
			requeue = after
		}
	}
	return requeue, nil
}

// listProxyReleases returns every release realizing proxy, across namespaces - matched by
// spec.packageProxyRef, never a name convention.
func (r *ZarfPackageProxyReconciler) listProxyReleases(ctx context.Context, proxy *addonsv1alpha1.ZarfPackageProxy) ([]*addonsv1alpha1.ZarfPackageRelease, error) {
	releases := &addonsv1alpha1.ZarfPackageReleaseList{}
	if err := r.List(ctx, releases); err != nil {
		return nil, err
	}
	var owned []*addonsv1alpha1.ZarfPackageRelease
	for i := range releases.Items {
		release := &releases.Items[i]
		refNS := release.Spec.PackageProxyRef.Namespace
		if refNS == "" {
			refNS = release.Namespace
		}
		if release.Spec.PackageProxyRef.Name == proxy.Name && refNS == proxy.Namespace {
			owned = append(owned, release)
		}
	}
	return owned, nil
}

// finalizeProxy handles a deleting proxy (§8B): delete its releases and wait for their own
// finalization - uninstall or orphan, per the policy - before the proxy's finalizer lets it go.
// Releases are deliberately not owner-referenced to the proxy (the Cluster is the sole owner,
// §7's cascade), so this finalizer is what makes proxy deletion reach them at all. The returned
// duration asks for a requeue when non-zero.
func (r *ZarfPackageProxyReconciler) finalizeProxy(ctx context.Context, proxy *addonsv1alpha1.ZarfPackageProxy) (time.Duration, error) {
	releases, err := r.listProxyReleases(ctx, proxy)
	if err != nil {
		return 0, err
	}
	for _, release := range releases {
		if !release.DeletionTimestamp.IsZero() {
			continue
		}
		if err := r.Delete(ctx, release); err != nil && !apierrors.IsNotFound(err) {
			return 0, err
		}
	}
	// Walk the just-deleted releases through their own finalization in the same pass; release
	// events keep the proxy requeued until the last one is gone.
	retry, err := r.finalizeReleases(ctx, proxy)
	if err != nil {
		return 0, err
	}
	if len(releases) > 0 {
		return retry, nil
	}
	if controllerutil.RemoveFinalizer(proxy, addonsv1alpha1.FinalizerReleaseCleanup) {
		return 0, r.Update(ctx, proxy)
	}
	return 0, nil
}

// finalizeRelease walks one deleting, removal-owing release toward letting go: run the removal
// Job, absorb its outcome, and drop the finalizer once the package is off the cluster - or
// immediately, wherever §7C says teardown outranks uninstall.
func (r *ZarfPackageProxyReconciler) finalizeRelease(ctx context.Context, proxy *addonsv1alpha1.ZarfPackageProxy, release *addonsv1alpha1.ZarfPackageRelease) (time.Duration, error) {
	log := ctrl.LoggerFrom(ctx)

	// §7C: a deleting or gone Cluster is infrastructure teardown; never uninstall toward it.
	cluster := &clusterv1.Cluster{}
	err := r.Get(ctx, client.ObjectKey{Namespace: release.Namespace, Name: release.Spec.ClusterRef.Name}, cluster)
	switch {
	case apierrors.IsNotFound(err), err == nil && !cluster.DeletionTimestamp.IsZero():
		return 0, r.dropRemovalFinalizer(ctx, release)
	case err != nil:
		return 0, err
	}

	if proxy.Status.PackageName == "" {
		// shortcut: no packageName means the proxy never resolved, so nothing was deployed
		// under a name this controller could remove; anything older than packageName predates
		// deploys entirely. Drop rather than block deletion on an unanswerable question.
		return 0, r.dropRemovalFinalizer(ctx, release)
	}

	before := release.DeepCopy()
	setReady := func(reason, message string) {
		release.Status.Phase = addonsv1alpha1.PhaseRemoving
		meta.SetStatusCondition(&release.Status.Conditions, metav1.Condition{
			Type:    addonsv1alpha1.ConditionReady,
			Status:  metav1.ConditionFalse,
			Reason:  reason,
			Message: message,
		})
	}

	job := &batchv1.Job{}
	err = r.Get(ctx, client.ObjectKey{Namespace: release.Namespace, Name: release.Name + "-remove"}, job)
	switch {
	case err == nil:
		switch DeployJobPhase(job, addonsv1alpha1.StrategyReconcile) {
		case addonsv1alpha1.PhaseDeploying:
			setReady(addonsv1alpha1.ReasonRemoving, "the removal Job is running")
			return 0, r.Status().Patch(ctx, release, client.MergeFrom(before))
		case addonsv1alpha1.PhaseFailed:
			result, _ := r.jobResult(ctx, job)
			message := result.Error
			if message == "" {
				message = "the removal Job failed and its pod left no termination message"
			}
			setReady(addonsv1alpha1.ReasonRemoving, "the removal Job failed; retrying")
			meta.SetStatusCondition(&release.Status.Conditions, metav1.Condition{
				Type:    addonsv1alpha1.ConditionRemovalFailed,
				Status:  metav1.ConditionTrue,
				Reason:  addonsv1alpha1.ReasonRemovalJobFailed,
				Message: message,
			})
			if err := r.Status().Patch(ctx, release, client.MergeFrom(before)); err != nil {
				return 0, err
			}
			// Delete the failed Job so the retry can stamp a fresh one after the delay.
			propagation := metav1.DeletePropagationBackground
			if err := r.Delete(ctx, job, &client.DeleteOptions{PropagationPolicy: &propagation}); err != nil && !apierrors.IsNotFound(err) {
				return 0, err
			}
			return removalRetryDelay, nil
		default:
			// Complete: the package is off the cluster. The Job garbage-collects with the
			// release once the finalizer goes.
			log.Info("removal succeeded; releasing the finalizer", "release", client.ObjectKeyFromObject(release))
			return 0, r.dropRemovalFinalizer(ctx, release)
		}
	case !apierrors.IsNotFound(err):
		return 0, err
	}

	// No Job yet. Probe for zarf's deployed-package record first: a release deleted before its
	// package ever deployed has nothing to remove, and an unreachable cluster is §7C teardown.
	workload, err := r.workloadClient(ctx, release.Namespace, cluster.Name)
	if err == nil {
		var deployed bool
		deployed, err = hasDeployedPackage(ctx, workload, proxy.Status.PackageName)
		if err == nil && !deployed {
			return 0, r.dropRemovalFinalizer(ctx, release)
		}
	}
	if err != nil {
		log.Info("the workload cluster is unreachable; dropping the finalizer without removal (§7C)",
			"release", client.ObjectKeyFromObject(release), "error", err.Error())
		return 0, r.dropRemovalFinalizer(ctx, release)
	}

	// §6 step 6's slot applies to removals too: one deploy or removal Job per cluster.
	holder, err := r.clusterSlotHolder(ctx, release.Namespace, cluster)
	if err != nil {
		return 0, err
	}
	if holder != "" {
		setReady(addonsv1alpha1.ReasonWaitingForActiveDeploy,
			fmt.Sprintf("Job %s holds the cluster's deploy slot", holder))
		return 0, r.Status().Patch(ctx, release, client.MergeFrom(before))
	}

	setReady(addonsv1alpha1.ReasonRemoving, "the removal Job is starting")
	if err := r.Status().Patch(ctx, release, client.MergeFrom(before)); err != nil {
		return 0, err
	}
	_, err = r.EnsureDeployJob(ctx, BuildRemovalJob(proxy, release, r.DeployImage, r.CachePath, proxy.Status.PackageName))
	return 0, err
}

// dropRemovalFinalizer lets a deleting release go.
func (r *ZarfPackageProxyReconciler) dropRemovalFinalizer(ctx context.Context, release *addonsv1alpha1.ZarfPackageRelease) error {
	if !controllerutil.RemoveFinalizer(release, addonsv1alpha1.FinalizerRemoval) {
		return nil
	}
	return r.Update(ctx, release)
}
