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
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"

	addonsv1alpha1 "github.com/colonel-byte/cluster-api-addon-provider-zarf/api/v1alpha1"
	"github.com/colonel-byte/cluster-api-addon-provider-zarf/internal/deployspec"
)

// stage is the normalized form every per-package step loops over: the declared spec.stages, or
// the single stage spec.package is sugar for. It resolves packageRef against spec.packages once,
// so no later code does that lookup, and it carries the two derived names - the Job suffix and
// the deploy-spec file name - so each has exactly one definition
// (docs/dev/design-package-stages.md, dist/plan-package-stages.md).
type stage struct {
	// name is the declared stage name; empty for the single-package form, whose Job and spec
	// file keep their pre-stages names so an upgrade renames nothing.
	name string

	// packageRefKey is the spec.packages key this stage resolved its source from; empty for
	// the single-package form, whose resolution lives in the flat status fields.
	packageRefKey string

	source     *addonsv1alpha1.ZarfPackageSource
	strategy   addonsv1alpha1.DeploymentStrategy
	components []addonsv1alpha1.ComponentSelector
	values     *apiextensionsv1.JSON
	set        map[string]string

	// jobSuffix is the buildJob suffix this stage's deploy Job carries: "-deploy" for the
	// single-package form, "-<name>-deploy" otherwise.
	jobSuffix string

	// specFileName is the Secret key this stage's rendered document sits under.
	specFileName string
}

// effectiveStages is the normalized stage list: one element per declared stage, or the single
// element spec.package is sugar for. Nothing in the reconciler branches on which form a proxy
// declared - a legacy proxy is the n=1 case of the only path. Returns nil when the spec declares
// neither form, which admission refuses but a test object can hold.
func effectiveStages(proxy *addonsv1alpha1.ZarfPackageProxy) []stage {
	spec := proxy.Spec
	if spec.Package != nil {
		strategy := spec.Strategy
		if strategy == "" {
			strategy = addonsv1alpha1.StrategyReconcile
		}
		return []stage{{
			source:       spec.Package,
			strategy:     strategy,
			components:   spec.Components,
			values:       spec.Values,
			set:          spec.Set,
			jobSuffix:    "-deploy",
			specFileName: deployspec.FileName,
		}}
	}
	stages := make([]stage, 0, len(spec.Stages))
	for _, declared := range spec.Stages {
		source, ok := spec.Packages[declared.PackageRef]
		if !ok {
			// Admission enforces the reference; a dangling one here is a test object, and a
			// stage with no source is not actionable.
			continue
		}
		strategy := declared.Strategy
		if strategy == "" {
			strategy = addonsv1alpha1.StrategyReconcile
		}
		stages = append(stages, stage{
			name:          declared.Name,
			packageRefKey: declared.PackageRef,
			source:        &source,
			strategy:      strategy,
			components:    declared.Components,
			values:        declared.Values,
			set:           declared.Set,
			jobSuffix:     "-" + declared.Name + "-deploy",
			specFileName:  deployspec.StageFileName(declared.Name),
		})
	}
	return stages
}

// packageResolution is a stage's resolution verdict: the flat status fields for the
// single-package form, the stage's status.packages entry otherwise. Nil until resolved.
func packageResolution(proxy *addonsv1alpha1.ZarfPackageProxy, st stage) *addonsv1alpha1.PackageResolution {
	if st.packageRefKey == "" {
		if proxy.Status.ResolvedDigest == "" {
			return nil
		}
		return &addonsv1alpha1.PackageResolution{
			ResolvedRef:    proxy.Status.ResolvedRef,
			ResolvedDigest: proxy.Status.ResolvedDigest,
			PackageName:    proxy.Status.PackageName,
			PackageKind:    proxy.Status.PackageKind,
		}
	}
	for i := range proxy.Status.Packages {
		entry := &proxy.Status.Packages[i]
		if entry.Name == st.packageRefKey && entry.ResolvedDigest != "" {
			return entry
		}
	}
	return nil
}

// initSource is the init package's source, whichever form declared it: the inline
// init.package, or the spec.packages entry init.packageRef names. Nil when the proxy
// declares no init package.
func initSource(proxy *addonsv1alpha1.ZarfPackageProxy) *addonsv1alpha1.ZarfPackageSource {
	if proxy.Spec.Init == nil {
		return nil
	}
	if proxy.Spec.Init.Package != nil {
		return proxy.Spec.Init.Package
	}
	if key := proxy.Spec.Init.PackageRef; key != "" {
		if source, ok := proxy.Spec.Packages[key]; ok {
			return &source
		}
	}
	return nil
}

// findStageState returns the release's applied record for the named stage, nil when none.
func findStageState(release *addonsv1alpha1.ZarfPackageRelease, name string) *addonsv1alpha1.StageState {
	for i := range release.Status.Stages {
		if release.Status.Stages[i].Name == name {
			return &release.Status.Stages[i]
		}
	}
	return nil
}

// upsertStageState writes a stage's applied record, replacing an existing one of the same name.
func upsertStageState(release *addonsv1alpha1.ZarfPackageRelease, record addonsv1alpha1.StageState) {
	if existing := findStageState(release, record.Name); existing != nil {
		*existing = record
		return
	}
	release.Status.Stages = append(release.Status.Stages, record)
}

// markUndeclaredStages flags applied records whose names the spec no longer declares. Kept
// rather than pruned: the record is the evidence an ApplyOnce decision rests on, and pruning
// it would make a re-declared stage of the same name run again.
func markUndeclaredStages(release *addonsv1alpha1.ZarfPackageRelease, stages []stage) {
	declared := map[string]bool{}
	for _, st := range stages {
		declared[st.name] = true
	}
	for i := range release.Status.Stages {
		release.Status.Stages[i].Undeclared = !declared[release.Status.Stages[i].Name]
	}
}

// declaredVersion is the version an operator reads as "what this proxy wants": the single
// package's declared version, or a staged proxy's last stage's - the end state the sequence
// converges on.
func declaredVersion(proxy *addonsv1alpha1.ZarfPackageProxy) string {
	if proxy.Spec.Package != nil {
		return proxy.Spec.Package.Version
	}
	stages := effectiveStages(proxy)
	if len(stages) == 0 {
		return ""
	}
	return stages[len(stages)-1].source.Version
}
