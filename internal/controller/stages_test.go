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
	"testing"

	addonsv1alpha1 "github.com/colonel-byte/cluster-api-addon-provider-zarf/api/v1alpha1"
)

// TestEffectiveStages is the normalization contract: a legacy proxy is the n=1 case of the only
// path, with the pre-stages Job suffix and file name so an upgrade renames nothing, and a staged
// proxy's list resolves each packageRef exactly once.
func TestEffectiveStages(t *testing.T) {
	legacy := &addonsv1alpha1.ZarfPackageProxy{}
	legacy.Spec.Package = &addonsv1alpha1.ZarfPackageSource{
		Repository: "registry.internal/app",
		Version:    "1.0.0",
	}

	applyOnce := legacy.DeepCopy()
	applyOnce.Spec.Strategy = addonsv1alpha1.StrategyApplyOnce

	staged := &addonsv1alpha1.ZarfPackageProxy{}
	staged.Spec.Packages = map[string]addonsv1alpha1.ZarfPackageSource{
		"zarf-init": {Repository: "registry.internal/init", Version: "v1.0.1"},
		"csi":       {Repository: "registry.internal/csi", Version: "v1.21.2"},
	}
	staged.Spec.Stages = []addonsv1alpha1.PackageStage{
		{
			Name:       "storage-seed",
			PackageRef: "csi",
			Strategy:   addonsv1alpha1.StrategyApplyOnce,
		},
		{
			Name:       "registry",
			PackageRef: "zarf-init",
		},
	}

	tests := []struct {
		name          string
		proxy         *addonsv1alpha1.ZarfPackageProxy
		wantCount     int
		wantFirstName string
		wantSuffixes  []string
		wantFiles     []string
		wantVersion   string
	}{
		{
			name:          "legacy proxy is the one-stage case with the pre-stages names",
			proxy:         legacy,
			wantCount:     1,
			wantFirstName: "",
			wantSuffixes:  []string{"-deploy"},
			wantFiles:     []string{"deploy-spec.yaml"},
			wantVersion:   "1.0.0",
		},
		{
			name:          "legacy ApplyOnce keeps its declared strategy",
			proxy:         applyOnce,
			wantCount:     1,
			wantFirstName: "",
			wantSuffixes:  []string{"-deploy"},
			wantFiles:     []string{"deploy-spec.yaml"},
			wantVersion:   "1.0.0",
		},
		{
			name:          "staged proxy resolves each packageRef in declared order",
			proxy:         staged,
			wantCount:     2,
			wantFirstName: "storage-seed",
			wantSuffixes:  []string{"-storage-seed-deploy", "-registry-deploy"},
			wantFiles:     []string{"stage-storage-seed.yaml", "stage-registry.yaml"},
			wantVersion:   "v1.0.1",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			stages := effectiveStages(test.proxy)
			if len(stages) != test.wantCount {
				t.Fatalf("stage count = %d, want %d", len(stages), test.wantCount)
			}
			if stages[0].name != test.wantFirstName {
				t.Errorf("first stage name = %q, want %q", stages[0].name, test.wantFirstName)
			}
			for i, stage := range stages {
				if stage.source == nil {
					t.Fatalf("stage %d has no source", i)
				}
				if stage.strategy == "" {
					t.Errorf("stage %d has no strategy; the default must be filled in", i)
				}
				if stage.jobSuffix != test.wantSuffixes[i] {
					t.Errorf("stage %d suffix = %q, want %q", i, stage.jobSuffix, test.wantSuffixes[i])
				}
				if stage.specFileName != test.wantFiles[i] {
					t.Errorf("stage %d file = %q, want %q", i, stage.specFileName, test.wantFiles[i])
				}
			}
			if got := declaredVersion(test.proxy); got != test.wantVersion {
				t.Errorf("declaredVersion = %q, want %q", got, test.wantVersion)
			}
		})
	}

	// A dangling packageRef - impossible past admission, reachable in a test object - drops the
	// stage rather than returning a stage with no source.
	dangling := staged.DeepCopy()
	dangling.Spec.Stages[1].PackageRef = "missing"
	if stages := effectiveStages(dangling); len(stages) != 1 {
		t.Errorf("dangling ref produced %d stages, want 1", len(stages))
	}
}

// TestStageDivergence pins option D's boundary: ApplyOnce history is immutable - renames,
// removals, and insertions before an applied seed hold - while Reconcile stages insert, remove
// and reorder freely, and appends are always the unambiguous edit.
func TestStageDivergence(t *testing.T) {
	appliedSeed := func() *addonsv1alpha1.ZarfPackageRelease {
		release := &addonsv1alpha1.ZarfPackageRelease{}
		release.Status.Stages = []addonsv1alpha1.StageState{
			{
				Name:     "seed",
				Strategy: addonsv1alpha1.StrategyApplyOnce,
			},
			{
				Name:     "final",
				Strategy: addonsv1alpha1.StrategyReconcile,
			},
		}
		return release
	}
	declare := func(entries ...stage) []stage { return entries }
	applyOnce := func(name string) stage {
		return stage{name: name, strategy: addonsv1alpha1.StrategyApplyOnce}
	}
	reconcile := func(name string) stage {
		return stage{name: name, strategy: addonsv1alpha1.StrategyReconcile}
	}

	tests := []struct {
		name         string
		stages       []stage
		wantDiverged bool
	}{
		{
			name:         "the declared list unchanged",
			stages:       declare(applyOnce("seed"), reconcile("final")),
			wantDiverged: false,
		},
		{
			name:         "an applied ApplyOnce stage renamed away",
			stages:       declare(applyOnce("seed-v2"), reconcile("final")),
			wantDiverged: true,
		},
		{
			name:         "an applied Reconcile stage removed",
			stages:       declare(applyOnce("seed")),
			wantDiverged: false,
		},
		{
			name:         "an ApplyOnce stage inserted before the applied seed",
			stages:       declare(applyOnce("pre-seed"), applyOnce("seed"), reconcile("final")),
			wantDiverged: true,
		},
		{
			name:         "an ApplyOnce stage appended after history",
			stages:       declare(applyOnce("seed"), reconcile("final"), applyOnce("late-seed")),
			wantDiverged: false,
		},
		{
			name:         "a Reconcile stage inserted anywhere",
			stages:       declare(reconcile("pre"), applyOnce("seed"), reconcile("mid"), reconcile("final")),
			wantDiverged: false,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			message := stageDivergence(appliedSeed(), test.stages)
			if (message != "") != test.wantDiverged {
				t.Errorf("divergence = %q, want diverged=%v", message, test.wantDiverged)
			}
		})
	}
}
