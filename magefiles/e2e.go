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
	"github.com/colonel-byte/cluster-api-addon-provider-zarf/magefiles/pkg/e2e"
	"github.com/magefile/mage/mg"
)

type (
	E2e mg.Namespace
)

// Up stands up the e2e topology: a kind management cluster with CAPI core + CAPD, one CAPD
// workload cluster, and a local OCI registry on the kind network. Needs docker, kind, and
// kubectl on PATH plus network access - a dev/CI surface, never part of the offline build.
func (E2e) Up() error {
	return e2e.Up()
}

// Down tears the e2e topology down, including the CAPD machine containers the management
// cluster created as siblings. Safe to run against a partial or absent topology.
func (E2e) Down() error {
	return e2e.Down()
}

// Run executes the e2e suite in the separate test/e2e module against the running topology.
// It requires e2e:up to have run; a bare `go test` in test/e2e has nothing to talk to.
func (E2e) Run() error {
	return e2e.Run()
}

// Move stands up a second management cluster, installs this provider into it, and pivots the
// topology onto it with `clusterctl move` - the migration an operator performs when they
// replace a management cluster. Its own target rather than part of e2e:run: it needs that
// second cluster with its own CAPI, CAPD and CAAPZ install, which roughly doubles the
// topology. Requires e2e:up and e2e:run to have run first, since what the pivot has to carry
// is the cluster and the packages those built.
func (E2e) Move() error {
	return e2e.Move()
}

// MoveDown removes only the move target's management cluster, leaving the rest of the topology
// as e2e:up built it.
func (E2e) MoveDown() error {
	return e2e.MoveDown()
}
