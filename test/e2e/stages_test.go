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
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	addonsv1alpha1 "github.com/colonel-byte/cluster-api-addon-provider-zarf/api/v1alpha1"
)

// The staged-proxy sequence (#379) against real clusters, over the two staged fixtures: each
// fixture is one component whose single ConfigMap carries a MARKER variable, so every stage
// that deploys the package upgrades the SAME Helm release with different set values. The
// shared release is the stand-in for the reference bundle's shared init/storage releases: a
// later stage's marker supersedes an earlier one's, and re-running a seed stage would revert
// it - which is exactly what must never happen.

const (
	stagePackageARepository = registryHost + "/packages/e2e-stage-a"
	stagePackageBRepository = registryHost + "/packages/e2e-stage-b"
	stagePackageVersion     = "0.1.0"
	stagePackageAName       = "e2e-stage-a"
	stagePackageBName       = "e2e-stage-b"
)

// stagedProxy declares the four-stage reference shape over the two staged fixtures: both
// seeds ApplyOnce, both finals Reconcile, every stage setting the shared release's marker.
func stagedProxy(name string) *addonsv1alpha1.ZarfPackageProxy {
	stageOf := func(stageName, ref string, strategy addonsv1alpha1.DeploymentStrategy, marker string) addonsv1alpha1.PackageStage {
		return addonsv1alpha1.PackageStage{
			Name:       stageName,
			PackageRef: ref,
			Strategy:   strategy,
			Set:        map[string]string{"MARKER": marker},
		}
	}
	return &addonsv1alpha1.ZarfPackageProxy{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: addonsv1alpha1.ZarfPackageProxySpec{
			ClusterSelector: metav1.LabelSelector{},
			DeletionPolicy:  addonsv1alpha1.PolicyRetain,
			Packages: map[string]addonsv1alpha1.ZarfPackageSource{
				"a": {Repository: stagePackageARepository, Version: stagePackageVersion, API: addonsv1alpha1.PackageAPIV1Alpha1},
				"b": {Repository: stagePackageBRepository, Version: stagePackageVersion, API: addonsv1alpha1.PackageAPIV1Alpha1},
			},
			Stages: []addonsv1alpha1.PackageStage{
				stageOf("a-seed", "a", addonsv1alpha1.StrategyApplyOnce, "a-seed"),
				stageOf("b-seed", "b", addonsv1alpha1.StrategyApplyOnce, "b-seed"),
				stageOf("a-final", "a", addonsv1alpha1.StrategyReconcile, "a-final"),
				stageOf("b-final", "b", addonsv1alpha1.StrategyReconcile, "b-final"),
			},
		},
	}
}

// stageMarker reads a staged fixture's ConfigMap marker off the workload cluster.
func stageMarker(ctx context.Context, workload client.Client, name string) (string, error) {
	configMap := &corev1.ConfigMap{}
	if err := workload.Get(ctx, types.NamespacedName{Namespace: "default", Name: name}, configMap); err != nil {
		return "", err
	}
	return configMap.Data["marker"], nil
}

// waitStagedConverged waits for a staged release to apply every declared stage.
func waitStagedConverged(t *testing.T, c client.Client, releaseKey types.NamespacedName) *addonsv1alpha1.ZarfPackageRelease {
	t.Helper()
	release := &addonsv1alpha1.ZarfPackageRelease{}
	waitLong(t, "the staged release never converged", func(ctx context.Context) (bool, error) {
		if err := c.Get(ctx, releaseKey, release); err != nil {
			return false, client.IgnoreNotFound(err)
		}
		phase, reason, err := releasePhase(ctx, c, releaseKey)
		if err != nil {
			return false, client.IgnoreNotFound(err)
		}
		if reason == addonsv1alpha1.ReasonDeployFailed {
			return false, terminalFailure(ctx, c, releaseKey, "a stage's deploy Job failed")
		}
		t.Logf("waiting for the staged release: phase=%s reason=%s applied=%d/%d",
			phase, reason, release.Status.AppliedStageCount, release.Status.StageCount)
		return release.Status.Phase == addonsv1alpha1.PhaseDeployed &&
			release.Status.AppliedStageCount == release.Status.StageCount, nil
	})
	return release
}

// TestStagedSequence is the staged happy path end to end: four stages run in declared order on
// a real cluster, the applied records land in order, both shared releases end on their final
// markers, and editing one stage re-runs that stage alone - the marker never passes back
// through a seed value, which is the destructive case the design exists to prevent.
func TestStagedSequence(t *testing.T) {
	c := phase1Client(t)
	workload := workloadClient(t)

	proxy := stagedProxy("staged-seq")
	deleteProxy(t, c, proxy)
	if err := c.Create(t.Context(), proxy); err != nil {
		t.Fatalf("creating the staged proxy: %v", err)
	}
	t.Cleanup(func() { deleteProxy(t, c, proxy) })

	releaseKey := types.NamespacedName{Namespace: "default", Name: "staged-seq-" + workloadCluster}
	release := waitStagedConverged(t, c, releaseKey)

	if len(release.Status.Stages) != 4 {
		t.Fatalf("applied records = %+v, want 4", release.Status.Stages)
	}
	wantOrder := []string{"a-seed", "b-seed", "a-final", "b-final"}
	for i, want := range wantOrder {
		if release.Status.Stages[i].Name != want {
			t.Fatalf("applied record %d = %q, want %q (records %+v)", i, release.Status.Stages[i].Name, want, release.Status.Stages)
		}
		if i > 0 && release.Status.Stages[i].AppliedAt.Before(&release.Status.Stages[i-1].AppliedAt) {
			t.Errorf("stage %s applied before its predecessor", want)
		}
	}

	for name, want := range map[string]string{stagePackageAName: "a-final", stagePackageBName: "b-final"} {
		marker, err := stageMarker(t.Context(), workload, name)
		if err != nil {
			t.Fatalf("reading %s's marker: %v", name, err)
		}
		if marker != want {
			t.Fatalf("%s marker = %q, want %q - the final stage did not supersede the seed", name, marker, want)
		}
	}

	// The seed stages' finished Jobs are the record of their runs; note their identities.
	seedJob := &batchv1.Job{}
	seedKey := types.NamespacedName{Namespace: "default", Name: "staged-seq-" + workloadCluster + "-a-seed-deploy"}
	if err := c.Get(t.Context(), seedKey, seedJob); err != nil {
		t.Fatalf("stage a-seed's Job is gone; an ApplyOnce stage's finished Job is its record: %v", err)
	}
	if seedJob.Labels["addons.colonel-byte.dev/stage"] != "a-seed" {
		t.Errorf("stage label = %q", seedJob.Labels["addons.colonel-byte.dev/stage"])
	}
	seedUID := seedJob.UID

	// Edit the last stage: it alone re-runs, and no marker reverts to a seed value.
	fresh := &addonsv1alpha1.ZarfPackageProxy{}
	if err := c.Get(t.Context(), types.NamespacedName{Namespace: "default", Name: "staged-seq"}, fresh); err != nil {
		t.Fatalf("re-reading the proxy: %v", err)
	}
	fresh.Spec.Stages[3].Set["MARKER"] = "b-final-2"
	if err := c.Update(t.Context(), fresh); err != nil {
		t.Fatalf("editing stage b-final: %v", err)
	}

	waitLong(t, "the edited stage never re-ran", func(ctx context.Context) (bool, error) {
		marker, err := stageMarker(ctx, workload, stagePackageBName)
		if err != nil {
			return false, client.IgnoreNotFound(err)
		}
		return marker == "b-final-2", nil
	})

	marker, err := stageMarker(t.Context(), workload, stagePackageAName)
	if err != nil {
		t.Fatalf("re-reading %s's marker: %v", stagePackageAName, err)
	}
	if marker != "a-final" {
		t.Fatalf("%s marker = %q after another stage's edit - a stage re-ran that must not have", stagePackageAName, marker)
	}
	if err := c.Get(t.Context(), seedKey, seedJob); err != nil {
		t.Fatalf("re-reading a-seed's Job: %v", err)
	}
	if seedJob.UID != seedUID {
		t.Fatal("stage a-seed's Job was replaced on another stage's edit - an ApplyOnce seed re-ran")
	}
}

// TestStagedDependsOnWaitsForAllStages proves a dependency on a staged proxy is all-or-nothing:
// the dependent holds while the staged proxy does not exist, and deploys only once every stage
// has applied - never after merely the first.
func TestStagedDependsOnWaitsForAllStages(t *testing.T) {
	c := phase1Client(t)

	dependent := &addonsv1alpha1.ZarfPackageProxy{
		ObjectMeta: metav1.ObjectMeta{Name: "staged-dependent", Namespace: "default"},
		Spec: addonsv1alpha1.ZarfPackageProxySpec{
			ClusterSelector: metav1.LabelSelector{},
			DeletionPolicy:  addonsv1alpha1.PolicyRetain,
			Package: &addonsv1alpha1.ZarfPackageSource{
				Repository: appPackageRepository,
				Version:    appPackageVersion,
				API:        addonsv1alpha1.PackageAPIV1Beta1,
			},
			DependsOn: []addonsv1alpha1.DependencyReference{{Name: "staged-dep"}},
		},
	}
	deleteProxy(t, c, dependent)
	if err := c.Create(t.Context(), dependent); err != nil {
		t.Fatalf("creating the dependent proxy: %v", err)
	}
	t.Cleanup(func() { deleteProxy(t, c, dependent) })

	// With the staged proxy absent, the dependent must hold - and keep holding.
	dependentKey := types.NamespacedName{Namespace: "default", Name: "staged-dependent-" + workloadCluster}
	waitLong(t, "the dependent release never reported its dependency hold", func(ctx context.Context) (bool, error) {
		release := &addonsv1alpha1.ZarfPackageRelease{}
		if err := c.Get(ctx, dependentKey, release); err != nil {
			return false, client.IgnoreNotFound(err)
		}
		return release.Status.Phase == addonsv1alpha1.PhasePending, nil
	})

	staged := stagedProxy("staged-dep")
	deleteProxy(t, c, staged)
	if err := c.Create(t.Context(), staged); err != nil {
		t.Fatalf("creating the staged proxy: %v", err)
	}
	t.Cleanup(func() { deleteProxy(t, c, staged) })

	stagedKey := types.NamespacedName{Namespace: "default", Name: "staged-dep-" + workloadCluster}
	waitStagedConverged(t, c, stagedKey)

	waitLong(t, "the dependent never deployed after the staged proxy converged", func(ctx context.Context) (bool, error) {
		phase, reason, err := releasePhase(ctx, c, dependentKey)
		if err != nil {
			return false, client.IgnoreNotFound(err)
		}
		if reason == addonsv1alpha1.ReasonDeployFailed {
			return false, terminalFailure(ctx, c, dependentKey, "the dependent's deploy Job failed")
		}
		t.Logf("waiting for the dependent: phase=%s reason=%s", phase, reason)
		return phase == string(addonsv1alpha1.PhaseDeployed), nil
	})

	// The staged release the dependent waited on reports every stage applied - what
	// all-or-nothing means in status.
	staged2 := &addonsv1alpha1.ZarfPackageRelease{}
	if err := c.Get(t.Context(), stagedKey, staged2); err != nil {
		t.Fatalf("re-reading the staged release: %v", err)
	}
	if staged2.Status.AppliedStageCount != staged2.Status.StageCount {
		t.Fatalf("the dependent deployed with %d/%d stages applied", staged2.Status.AppliedStageCount, staged2.Status.StageCount)
	}
}
