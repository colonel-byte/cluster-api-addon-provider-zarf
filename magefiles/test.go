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

//go:build mage
// +build mage

package main

import (
	"path/filepath"

	"github.com/magefile/mage/mg"
	"github.com/magefile/mage/sh"
)

type (
	Test mg.Namespace
)

// envtestK8sVersion is the Kubernetes version of the control-plane binaries setup-envtest
// fetches for test:envtest. The binaries are released as `envtest-v<version>` tags on
// kubernetes-sigs/controller-tools, which is what the annotation tracks.
//
// renovate: datasource=github-releases depName=kubernetes-sigs/controller-tools extractVersion=^envtest-v(?<version>.+)$
const envtestK8sVersion = "1.34.1"

// capiCRDDir is where test:envtest renders Cluster API's core CRDs. Under dist/ because it is
// build output: regenerated on every run, ignored by git.
const capiCRDDir = "dist/envtest-crds"

// Envtest runs the tests that need a real API server - everything behind the envtest build tag.
//
// Cluster API's CRDs are generated from the vendored types rather than copied out of the
// cluster-api module, whose config/crd directory `go mod vendor` drops: generating needs no
// network and no module cache, and the schemas the tests run against are the ones the vendored
// types actually describe. The control-plane binaries do come from the network on first use;
// this target is a developer/CI surface, not part of the offline release build.
func (Test) Envtest() error {
	if err := sh.RunV("go", "tool", "controller-gen", "crd",
		"paths=./vendor/sigs.k8s.io/cluster-api/api/core/v1beta2/...",
		"output:crd:artifacts:config="+capiCRDDir); err != nil {
		return err
	}
	assets, err := sh.Output("go", "tool", "setup-envtest", "use", envtestK8sVersion, "-p", "path")
	if err != nil {
		return err
	}
	// Absolute, because a test binary runs with its own package directory as the working
	// directory and would resolve a relative path against that.
	crdDir, err := filepath.Abs(capiCRDDir)
	if err != nil {
		return err
	}
	return sh.RunWithV(map[string]string{
		"CAPI_CRD_DIR":       crdDir,
		"KUBEBUILDER_ASSETS": assets,
	}, "go", "test", "-race", "-tags=envtest", "./api/...", "./cmd/...", "./internal/...")
}
