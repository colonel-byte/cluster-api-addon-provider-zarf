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
