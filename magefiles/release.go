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
	"github.com/colonel-byte/cluster-api-addon-provider-zarf/magefiles/pkg/release"
	"github.com/magefile/mage/mg"
)

type (
	Release mg.Namespace
)

// Manifests renders out/addon-components.yaml - the component manifest clusterctl installs,
// with the controller image pinned at the given tag - and stages out/metadata.yaml beside it.
// goreleaser runs it as a before hook at the release version; run it with `dev` locally.
func (Release) Manifests(tag string) error {
	return release.Manifests(tag)
}

// Image builds the controller image at the release reference for the given tag, locally and for
// the host architecture only - what the install smoke test kind-loads. Publishing stays with
// goreleaser's multi-arch build.
func (Release) Image(tag string) error {
	return release.Image(tag)
}

// ZarfPackage builds the air-gapped Zarf package into out/, at the version zarf.yaml pins -
// never at a tag the caller picks, so the manifest and the image cannot disagree. Needs the
// release's images to be published and the zarf CLI on PATH; ZARF_ARCH cross-builds and a
// COSIGN_KEY in the environment signs. See docs/agent/choice-zarf-package.md.
func (Release) ZarfPackage() error {
	return release.ZarfPackage()
}

// OciArtifact packs the component manifests at the given tag into an OCI image layout under
// out/oci (plus a travel tar), which is what the Cluster API Operator and Rancher Turtles
// install from inside an air gap. See docs/guides/rancher-turtles.md.
func (Release) OciArtifact(tag string) error {
	return release.OCIArtifact(tag)
}
