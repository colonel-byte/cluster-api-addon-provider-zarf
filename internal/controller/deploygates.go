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
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	addonsv1alpha1 "github.com/colonel-byte/cluster-api-addon-provider-zarf/api/v1alpha1"
	"github.com/colonel-byte/cluster-api-addon-provider-zarf/internal/deployspec"
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
		if err := r.patchStatus(ctx, release, before); err != nil {
			return err
		}
	}
	return nil
}

// applyOnceSupersededMessage reports whether an applied ApplyOnce release has been left behind
// by its proxy - the declared version is no longer the one this cluster was seeded with - and
// the message that says so. False means the declaration and the cluster agree, where there is
// nothing to report and the release's existing reason is the better record.
func applyOnceSupersededMessage(proxy *addonsv1alpha1.ZarfPackageProxy, release *addonsv1alpha1.ZarfPackageRelease) (string, bool) {
	seeded := release.Status.DeployedVersion
	declared := declaredVersion(proxy)
	if seeded == "" || declared == "" || declared == seeded {
		return "", false
	}
	return fmt.Sprintf("this cluster was seeded with %s and ApplyOnce never re-executes, so the declared %s will not deploy here - switch the strategy to Reconcile, or re-seed deliberately",
		seeded, declared), true
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

	stages := effectiveStages(proxy)
	if len(stages) == 0 {
		return nil, nil
	}

	// §3's first tier: an applied ApplyOnce release never round-trips anywhere again.
	//
	// It speaks up only when the proxy has been bumped since the seed, which is the state an
	// operator cannot otherwise see: the release reports Completed either way, and the only
	// difference between "the declared version is deployed here" and "the declared version will
	// never deploy here" is two fields compared by eye. When they agree there is nothing to add,
	// and overwriting whatever reason recorded how this release completed - an adoption, say -
	// would lose the more specific fact and re-write the condition on every pass.
	if proxy.Spec.Package != nil && proxy.Spec.Strategy == addonsv1alpha1.StrategyApplyOnce && release.Status.Applied {
		release.Status.Phase = addonsv1alpha1.PhaseCompleted
		if message, diverged := applyOnceSupersededMessage(proxy, release); diverged {
			setReady(metav1.ConditionTrue, addonsv1alpha1.ReasonAlreadySeeded, message)
		}
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

	if proxy.Spec.Package == nil {
		return r.convergeStages(ctx, proxy, cluster, release, stages, setReady)
	}
	st := stages[0]

	// An existing Job blocks a new one: running means Deploying, finished means its outcome is
	// absorbed into status - and a stale finished Job is deleted there, so the next pass
	// stamps the redeploy.
	existing := &batchv1.Job{}
	err := r.Get(ctx, client.ObjectKey{Namespace: release.Namespace, Name: jobName(release.Name, "-deploy")}, existing)
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

	// The init-settings guard: record the cluster's own registry mode for kubectl, and refuse
	// to deploy a ZarfInitConfig whose declared mode the cluster's state contradicts.
	hold, err := r.guardInitSettings(ctx, workload, proxy, release,
		proxy.Status.PackageKind == "ZarfInitConfig", setReady)
	if err != nil {
		return nil, err
	}
	if hold {
		return nil, nil
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
	// slot - a package resolving as ZarfInitConfig deploys as-is, an autoInit release stamps
	// its init Job, and everything else holds. No carve-out for stateless packages: v1alpha1's
	// metadata.yolo is deprecated with no v1beta1 successor, so "needs no init" becomes a
	// deploy-time connectivity knob on the proxy when one is designed - never package metadata.
	if proxy.Status.PackageKind != "ZarfInitConfig" {
		hasState, err := hasZarfState(ctx, workload)
		if err != nil {
			return nil, err
		}
		if !hasState {
			return r.convergeInit(ctx, proxy, cluster, release, setReady)
		}
		// The cluster has state, but that is not on its own proof the init finished: zarf
		// writes state early and can fail after, leaving a cluster with state and no registry.
		// So an auto-init release settles its own init Job before anything deploys.
		if proxy.Spec.Init != nil && proxy.Spec.Init.AutoInit {
			hold, err := r.settleInitJob(ctx, release, setReady)
			if err != nil {
				return nil, err
			}
			if hold {
				return nil, nil
			}
		}
	}

	// §6 step 6: one deploy or removal Job per cluster at a time, unless the Cluster opts out.
	holder, err := r.clusterSlotHolder(ctx, release.Namespace, cluster)
	if err != nil {
		return nil, err
	}
	if holder != "" {
		release.Status.Phase = addonsv1alpha1.PhasePending
		setReady(metav1.ConditionFalse, addonsv1alpha1.ReasonWaitingForActiveDeploy,
			fmt.Sprintf("Job %s holds the cluster's deploy slot", holder))
		return nil, nil
	}

	release.Status.Phase = addonsv1alpha1.PhaseDeploying
	setReady(metav1.ConditionFalse, addonsv1alpha1.ReasonDeploying, "the deploy Job is starting")
	job := BuildDeployJob(proxy, release, r.DeployImage, r.CachePath, st)
	// The spec hash is what later marks this Job stale once an edit re-renders the spec.
	if hash := r.specHash(ctx, release, st.specFileName); hash != "" {
		job.Annotations = map[string]string{specHashAnnotation: hash}
	}
	return job, nil
}

// guardInitSettings compares the declared init registry mode against the one the cluster's own
// zarf-state records, writes the observed mode onto the release so `kubectl get
// zarfpackagereleases -o wide` shows it, and raises the InitSettingsSuperseded condition on a
// disagreement. The returned hold is true only when modePassing - this release is about to
// deploy a ZarfInitConfig that would carry the conflicting mode, which upstream would enact.
// A release that merely declares a superseded mode keeps deploying; the condition is its record.
func (r *ZarfPackageProxyReconciler) guardInitSettings(ctx context.Context, workload client.Client, proxy *addonsv1alpha1.ZarfPackageProxy, release *addonsv1alpha1.ZarfPackageRelease, modePassing bool, setReady func(metav1.ConditionStatus, string, string)) (bool, error) {
	observed, err := clusterRegistryMode(ctx, workload)
	if err != nil {
		return false, err
	}
	release.Status.ObservedRegistryMode = observed

	declared := ""
	if proxy.Spec.Init != nil && proxy.Spec.Init.Registry != nil {
		declared = string(proxy.Spec.Init.Registry.Mode)
	}
	if declared == "" || observed == "" || declared == observed {
		meta.RemoveStatusCondition(&release.Status.Conditions, addonsv1alpha1.ConditionInitSettingsSuperseded)
		return false, nil
	}

	message := fmt.Sprintf("the cluster's zarf-state records registry mode %q and this proxy declares %q; registry mode is immutable per cluster, so the declared mode will not be applied here - another proxy or an earlier init owns it", observed, declared)
	meta.SetStatusCondition(&release.Status.Conditions, metav1.Condition{
		Type:    addonsv1alpha1.ConditionInitSettingsSuperseded,
		Status:  metav1.ConditionTrue,
		Reason:  addonsv1alpha1.ReasonRegistryModeMismatch,
		Message: message,
	})
	if !modePassing {
		return false, nil
	}
	release.Status.Phase = addonsv1alpha1.PhasePending
	setReady(metav1.ConditionFalse, addonsv1alpha1.ReasonRegistryModeMismatch, message)
	return true, nil
}

// convergeStages is the staged form's ordering walk (docs/dev/design-package-stages.md): each
// stage is settled (its applied record matches the rendered spec, or it is ApplyOnce with any
// record), in flight (its labelled Job is running), or owed - and the first owed stage whose
// predecessors are all settled takes the cluster's deploy slot. Only changed stages re-run, in
// declared order. Job creation is the commit point: in-flight work is reconstructed from the
// stage-labelled Jobs, never from status, so a controller restart between creating a Job and
// writing status recovers rather than guessing - and for an ApplyOnce seed stage guessing
// wrong is the destructive direction.
func (r *ZarfPackageProxyReconciler) convergeStages(ctx context.Context, proxy *addonsv1alpha1.ZarfPackageProxy, cluster *clusterv1.Cluster, release *addonsv1alpha1.ZarfPackageRelease, stages []stage, setReady func(metav1.ConditionStatus, string, string)) (*batchv1.Job, error) {
	markUndeclaredStages(release, stages)

	if divergence := stageDivergence(release, stages); divergence != "" {
		release.Status.Phase = addonsv1alpha1.PhasePending
		setReady(metav1.ConditionFalse, addonsv1alpha1.ReasonStageListDiverged, divergence)
		return nil, nil
	}

	// The counts behind the printer columns: declared stages stamped from the proxy so the
	// release self-describes after a clusterctl move, settled ones counted as the walk passes
	// them - whichever return ends this pass.
	applied := 0
	release.Status.StageCount = int32(len(stages))
	defer func() {
		release.Status.AppliedStageCount = int32(applied)
	}()

	jobs := &batchv1.JobList{}
	if err := r.List(ctx, jobs, client.InNamespace(release.Namespace),
		client.MatchingLabels{releaseLabel: release.Name}); err != nil {
		return nil, err
	}
	byStage := map[string]*batchv1.Job{}
	for i := range jobs.Items {
		if name := jobs.Items[i].Labels[stageLabel]; name != "" {
			byStage[name] = &jobs.Items[i]
		}
	}

	// Probes are rare; the workload client dials lazily so a walk that never probes never dials.
	var workload client.Client
	workloadFor := func() (client.Client, error) {
		if workload != nil {
			return workload, nil
		}
		var err error
		workload, err = r.workloadClient(ctx, release.Namespace, cluster.Name)
		return workload, err
	}

	for _, st := range stages {
		desired := r.specHash(ctx, release, st.specFileName)

		if job := byStage[st.name]; job != nil {
			switch DeployJobPhase(job, st.strategy) {
			case addonsv1alpha1.PhaseDeploying:
				release.Status.Phase = addonsv1alpha1.PhaseDeploying
				setReady(metav1.ConditionFalse, addonsv1alpha1.ReasonDeploying,
					fmt.Sprintf("stage %s's deploy Job is running", st.name))
				return nil, nil
			case addonsv1alpha1.PhaseFailed:
				result, _ := r.jobResult(ctx, job)
				message := result.Error
				if message == "" {
					message = fmt.Sprintf("stage %s's deploy Job failed and its pod left no termination message", st.name)
				}
				release.Status.Phase = addonsv1alpha1.PhaseFailed
				setReady(metav1.ConditionFalse, addonsv1alpha1.ReasonDeployFailed, message)
				// Edit-to-retry, the single-package rule: a failed stage holds the sequence
				// until its spec changes, at which point the stale Job is deleted and the walk
				// re-runs it. A failed ApplyOnce stage never applied, so re-running it on an
				// edit re-attempts the seed rather than re-executing applied work.
				if desired != "" && job.Annotations[specHashAnnotation] != desired {
					propagation := metav1.DeletePropagationBackground
					if err := r.Delete(ctx, job, &client.DeleteOptions{PropagationPolicy: &propagation}); err != nil && !apierrors.IsNotFound(err) {
						return nil, err
					}
				}
				return nil, nil
			default:
				// Finished successfully: absorb into the applied record. A stale Reconcile
				// stage's Job is deleted so the walk re-stamps it; an ApplyOnce stage's
				// finished Job stays as the record of the run.
				r.absorbStageJob(ctx, release, st, job)
				if st.strategy == addonsv1alpha1.StrategyReconcile && desired != "" &&
					job.Annotations[specHashAnnotation] != desired {
					propagation := metav1.DeletePropagationBackground
					if err := r.Delete(ctx, job, &client.DeleteOptions{PropagationPolicy: &propagation}); err != nil && !apierrors.IsNotFound(err) {
						return nil, err
					}
				}
			}
		}

		record := findStageState(release, st.name)
		if record != nil && (st.strategy == addonsv1alpha1.StrategyApplyOnce ||
			(desired != "" && record.SpecHash == desired)) {
			applied++
			continue
		}

		if desired == "" {
			// The stage's document is not rendered - its package has not resolved - so there
			// is no identity to deploy or compare. Hold; the proxy's conditions say why.
			release.Status.Phase = addonsv1alpha1.PhasePending
			setReady(metav1.ConditionFalse, addonsv1alpha1.ReasonWaitingForStageSpec,
				fmt.Sprintf("stage %s's document is not rendered; see the proxy's PackageResolved condition", st.name))
			return nil, nil
		}

		resolution := packageResolution(proxy, st)

		// Adoption, option B: an ApplyOnce stage with no record is satisfied by the cluster's
		// own deployed-package record - the probe the single-package ApplyOnce path already
		// uses. Deciding "satisfied" wrongly fails a later stage loudly; deciding "not
		// satisfied" wrongly re-applies seed values over a converged cluster, so ambiguity
		// leans towards satisfied.
		if st.strategy == addonsv1alpha1.StrategyApplyOnce && resolution != nil && resolution.PackageName != "" {
			w, err := workloadFor()
			if err != nil {
				return nil, err
			}
			adopted, err := hasDeployedPackage(ctx, w, resolution.PackageName)
			if err != nil {
				return nil, err
			}
			if adopted {
				upsertStageState(release, addonsv1alpha1.StageState{
					Name:      st.name,
					SpecHash:  desired,
					Strategy:  st.strategy,
					AppliedAt: metav1.Now(),
				})
				applied++
				continue
			}
		}

		// The init-settings guard, before an init-capable stage may carry its mode to the
		// cluster - and the observed-mode record for kubectl either way.
		{
			w, err := workloadFor()
			if err != nil {
				return nil, err
			}
			hold, err := r.guardInitSettings(ctx, w, proxy, release,
				resolution != nil && resolution.PackageKind == "ZarfInitConfig", setReady)
			if err != nil {
				return nil, err
			}
			if hold {
				return nil, nil
			}
		}

		// §6 step 5 per stage: a stage that is not itself init-capable needs zarf state first.
		if resolution == nil || resolution.PackageKind != "ZarfInitConfig" {
			w, err := workloadFor()
			if err != nil {
				return nil, err
			}
			hasState, err := hasZarfState(ctx, w)
			if err != nil {
				return nil, err
			}
			if !hasState {
				return r.convergeInit(ctx, proxy, cluster, release, setReady)
			}
			if proxy.Spec.Init != nil && proxy.Spec.Init.AutoInit {
				hold, err := r.settleInitJob(ctx, release, setReady)
				if err != nil {
					return nil, err
				}
				if hold {
					return nil, nil
				}
			}
		}

		holder, err := r.clusterSlotHolder(ctx, release.Namespace, cluster)
		if err != nil {
			return nil, err
		}
		if holder != "" {
			release.Status.Phase = addonsv1alpha1.PhasePending
			setReady(metav1.ConditionFalse, addonsv1alpha1.ReasonWaitingForActiveDeploy,
				fmt.Sprintf("Job %s holds the cluster's deploy slot", holder))
			return nil, nil
		}

		release.Status.Phase = addonsv1alpha1.PhaseDeploying
		setReady(metav1.ConditionFalse, addonsv1alpha1.ReasonDeploying,
			fmt.Sprintf("stage %s's deploy Job is starting", st.name))
		job := BuildDeployJob(proxy, release, r.DeployImage, r.CachePath, st)
		job.Annotations = map[string]string{specHashAnnotation: desired}
		return job, nil
	}

	release.Status.Phase = addonsv1alpha1.PhaseDeployed
	setReady(metav1.ConditionTrue, addonsv1alpha1.ReasonDeployed,
		fmt.Sprintf("all %d stages applied", len(stages)))
	return nil, nil
}

// absorbStageJob writes a finished stage Job's outcome into the release's applied record. The
// identity recorded is the hash the Job was stamped from - the controller's own hash of the
// rendered spec, never the pod-reported digest, which a lost termination message leaves empty.
// Idempotent: a record already carrying the Job's hash is left alone, keeping its appliedAt.
func (r *ZarfPackageProxyReconciler) absorbStageJob(ctx context.Context, release *addonsv1alpha1.ZarfPackageRelease, st stage, job *batchv1.Job) {
	ranHash := job.Annotations[specHashAnnotation]
	if existing := findStageState(release, st.name); existing != nil && existing.SpecHash == ranHash {
		return
	}
	record := addonsv1alpha1.StageState{
		Name:      st.name,
		SpecHash:  ranHash,
		Strategy:  st.strategy,
		AppliedAt: metav1.Now(),
	}
	if t := job.Status.CompletionTime; t != nil {
		record.AppliedAt = *t
	}
	if result, found := r.jobResult(ctx, job); found {
		if i := strings.LastIndex(result.Package, "@"); i >= 0 {
			record.DeployedDigest = result.Package[i+1:]
		}
		record.DeployedVersion = result.Version
	}
	upsertStageState(release, record)
}

// convergeInit drives §6 step 5's auto-init deploy path for one release on a state-less
// cluster: stamp the init Job when the release declares autoInit and the init package is
// resolved, track a running or failed one, and hold everything else with WaitingForZarfState.
// The returned Job follows convergeDeploy's contract: nil with nil error means the release holds.
func (r *ZarfPackageProxyReconciler) convergeInit(ctx context.Context, proxy *addonsv1alpha1.ZarfPackageProxy, cluster *clusterv1.Cluster, release *addonsv1alpha1.ZarfPackageRelease, setReady func(metav1.ConditionStatus, string, string)) (*batchv1.Job, error) {
	if proxy.Spec.Init == nil || !proxy.Spec.Init.AutoInit {
		release.Status.Phase = addonsv1alpha1.PhasePending
		setReady(metav1.ConditionFalse, addonsv1alpha1.ReasonWaitingForZarfState,
			"the cluster has no zarf state; only an init-capable release may deploy first")
		return nil, nil
	}
	if !meta.IsStatusConditionTrue(proxy.Status.Conditions, addonsv1alpha1.ConditionInitPackageResolved) {
		release.Status.Phase = addonsv1alpha1.PhasePending
		setReady(metav1.ConditionFalse, addonsv1alpha1.ReasonWaitingForZarfState,
			"the cluster has no zarf state and the init package is not resolved; see the proxy's InitPackageResolved condition")
		return nil, nil
	}

	existing := &batchv1.Job{}
	err := r.Get(ctx, client.ObjectKey{Namespace: release.Namespace, Name: jobName(release.Name, "-init")}, existing)
	switch {
	case err == nil:
		if DeployJobPhase(existing, addonsv1alpha1.StrategyReconcile) == addonsv1alpha1.PhaseDeploying {
			release.Status.Phase = addonsv1alpha1.PhaseDeploying
			setReady(metav1.ConditionFalse, addonsv1alpha1.ReasonInitializing,
				"the init Job is deploying the init package")
			return nil, nil
		}
		return nil, r.absorbFinishedInitJob(ctx, release, existing)
	case !apierrors.IsNotFound(err):
		return nil, err
	}

	// §6 step 6 applies to init Jobs like any other: two autoInit proxies racing on a fresh
	// cluster serialize here, and the loser's next state probe no-ops.
	holder, err := r.clusterSlotHolder(ctx, release.Namespace, cluster)
	if err != nil {
		return nil, err
	}
	if holder != "" {
		release.Status.Phase = addonsv1alpha1.PhasePending
		setReady(metav1.ConditionFalse, addonsv1alpha1.ReasonWaitingForActiveDeploy,
			fmt.Sprintf("Job %s holds the cluster's deploy slot", holder))
		return nil, nil
	}

	release.Status.Phase = addonsv1alpha1.PhaseDeploying
	setReady(metav1.ConditionFalse, addonsv1alpha1.ReasonInitializing, "the init Job is starting")
	job := BuildInitJob(proxy, release, r.DeployImage, r.CachePath)
	if hash := r.specHash(ctx, release, deployspec.InitFileName); hash != "" {
		job.Annotations = map[string]string{specHashAnnotation: hash}
	}
	return job, nil
}

// settleInitJob decides what an auto-init release's own init Job means now that its cluster
// carries zarf state, and reports whether the release holds instead of deploying.
//
// A completed Job is scaffolding - the state is the record - so it is deleted and the release
// proceeds. A *failed* one is the case worth care: zarf writes state before it finishes, so a
// failed init leaves a cluster that has state and may have no registry, and there is no way
// from here to tell that apart from a cluster someone else initialized properly. Holding with
// the failure visible is the honest answer; deploying onto it would bury the failure under a
// deploy that reads as success. A running Job holds the §6 step 6 slot until it finishes.
func (r *ZarfPackageProxyReconciler) settleInitJob(ctx context.Context, release *addonsv1alpha1.ZarfPackageRelease, setReady func(metav1.ConditionStatus, string, string)) (bool, error) {
	job := &batchv1.Job{}
	err := r.Get(ctx, client.ObjectKey{Namespace: release.Namespace, Name: jobName(release.Name, "-init")}, job)
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}

	switch DeployJobPhase(job, addonsv1alpha1.StrategyReconcile) {
	case addonsv1alpha1.PhaseDeploying:
		release.Status.Phase = addonsv1alpha1.PhaseDeploying
		setReady(metav1.ConditionFalse, addonsv1alpha1.ReasonInitializing,
			"the init Job is deploying the init package")
		return true, nil
	case addonsv1alpha1.PhaseFailed:
		return true, r.absorbFinishedInitJob(ctx, release, job)
	}

	propagation := metav1.DeletePropagationBackground
	if err := r.Delete(ctx, job, &client.DeleteOptions{PropagationPolicy: &propagation}); err != nil && !apierrors.IsNotFound(err) {
		return false, err
	}
	return false, nil
}

// clusterSlotHolder returns the name of the deploy or removal Job currently running against
// cluster - the holder of §6 step 6's one-Job-per-cluster slot - or "". Always "" when the
// Cluster's concurrent-deploys annotation opts it out of the serialization.
func (r *ZarfPackageProxyReconciler) clusterSlotHolder(ctx context.Context, namespace string, cluster *clusterv1.Cluster) (string, error) {
	if cluster.Annotations[addonsv1alpha1.AnnotationConcurrentDeploys] == "true" {
		return "", nil
	}
	jobs := &batchv1.JobList{}
	if err := r.List(ctx, jobs, client.InNamespace(namespace),
		client.MatchingLabels{clusterLabel: cluster.Name}); err != nil {
		return "", err
	}
	for j := range jobs.Items {
		if DeployJobPhase(&jobs.Items[j], addonsv1alpha1.StrategyReconcile) == addonsv1alpha1.PhaseDeploying {
			return jobs.Items[j].Name, nil
		}
	}
	return "", nil
}
