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

// Package release renders the artifacts a release publishes: the component manifest clusterctl
// installs and - later targets - the Zarf package. The shape mirrors cluster-api-provider-
// cargoship's `make release-manifests`, with mage in place of make and `go tool kustomize` in
// place of a downloaded binary.
package release

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// Dir is where release artifacts land. Gitignored build output, recreated on every render.
const Dir = "out"

// ControllerImage is the controller image reference the release pipeline publishes, without a
// tag. The manifests and the Zarf package both pin their tag onto it.
const ControllerImage = "ghcr.io/colonel-byte/cluster-api-addon-provider-zarf-controller"

// ImageRef returns the controller image reference at the given tag.
func ImageRef(tag string) string {
	return ControllerImage + ":" + tag
}

// Manifests renders out/addon-components.yaml - the component manifest clusterctl installs,
// with the controller image pinned at tag - and stages out/metadata.yaml beside it. The deploy
// manifests and the Zarf package deploy this same artifact, never a second kustomize assembly,
// so what an operator installs by hand and what the package installs cannot drift.
func Manifests(tag string) error {
	if tag == "" {
		return fmt.Errorf("a tag is required: release:manifests <tag>")
	}
	if err := os.RemoveAll(Dir); err != nil {
		return err
	}
	if err := os.MkdirAll(Dir, 0o755); err != nil {
		return err
	}

	// `kustomize edit` rewrites config/manager/kustomization.yaml in place, the same working-
	// tree mutation cargoship's make target performs; a local run leaves the pinned tag behind
	// for git to show.
	edit := exec.Command("go", "tool", "kustomize", "edit", "set", "image", "controller="+ImageRef(tag))
	edit.Dir = filepath.Join("config", "manager")
	edit.Stdout, edit.Stderr = os.Stdout, os.Stderr
	if err := edit.Run(); err != nil {
		return fmt.Errorf("pinning the controller image: %w", err)
	}

	build := exec.Command("go", "tool", "kustomize", "build", filepath.Join("config", "default"),
		"-o", filepath.Join(Dir, "addon-components.yaml"))
	build.Stdout, build.Stderr = os.Stdout, os.Stderr
	if err := build.Run(); err != nil {
		return fmt.Errorf("rendering the component manifest: %w", err)
	}

	metadata, err := os.ReadFile("metadata.yaml")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(Dir, "metadata.yaml"), metadata, 0o644) //nolint:gosec // manifest
}
