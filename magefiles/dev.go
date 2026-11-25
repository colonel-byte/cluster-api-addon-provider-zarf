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
	"fmt"

	"github.com/colonel-byte/cluster-api-addon-provider-zarf/magefiles/pkg/osv"
	"github.com/colonel-byte/cluster-api-addon-provider-zarf/magefiles/pkg/util"
	"github.com/magefile/mage/mg"
	"github.com/magefile/mage/sh"
)

type (
	Dev mg.Namespace
)

// Vendor re-resolves the module graph and rewrites vendor/ from it, then writes the
// osv-scanner.toml overrides back into the tree it just deleted them from. This repository
// builds from vendor/ and is expected to build with no network at all, so a dependency change
// is not done until this has run and the result is committed - see AGENTS.md.
func (d Dev) Vendor() error {
	if err := sh.RunV("go", "mod", "tidy"); err != nil {
		return err
	}
	if err := sh.RunV("go", "mod", "vendor"); err != nil {
		return err
	}
	return d.WriteOSVOverrides()
}

// WriteOSVOverrides writes an osv-scanner.toml beside every non-Go dependency manifest `go mod
// vendor` copies into vendor/, which is what keeps the Scorecard Vulnerabilities check reporting
// on CAAPZ rather than on the Python tooling its Go dependencies ship in their own repositories.
// Separate from Vendor because the things that delete these files - Renovate re-vendoring on a
// bump, CI re-vendoring to diff - do not want the tidy that comes with it. See
// docs/agent/choice-osv-vendor-overrides.md.
func (Dev) WriteOSVOverrides() error {
	fmt.Println("Writing vendored osv-scanner overrides")
	return osv.WriteOverrides()
}

// VerifyVendor fails if go.mod, go.sum, or vendor/ is not what the module graph implies - which
// is what a dependency added without re-vendoring looks like. `go mod verify` checks the module
// cache rather than the vendor tree, so this runs the consistency check the toolchain does for
// a vendored build and reports it as a failure rather than as a build error somewhere later.
func (Dev) VerifyVendor() error {
	fmt.Println("Verifying vendor/ is consistent with go.mod")
	if err := sh.RunV("go", "mod", "verify"); err != nil {
		return err
	}
	// -mod=vendor with no network: this is the invocation a release has to survive, so it is
	// the one to fail on.
	if err := sh.RunWithV(map[string]string{"GOFLAGS": "-mod=vendor", "GOPROXY": "off"},
		"go", "build", "./..."); err != nil {
		return err
	}
	fmt.Println("Verifying vendored osv-scanner overrides")
	return osv.VerifyVendor()
}

// CheckTables checks that every Markdown table in the tree is padded the way the style guide
// in AGENTS.md requires, and prints the expected form of each line that is not. The root
// AGENTS.md (and CLAUDE.md, the symlink to it) are exempt.
func (Dev) CheckTables() error {
	fmt.Println("Checking markdown table padding")
	return util.CheckRepoMarkdownTables()
}
