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

// Package config holds the build-time identity goreleaser stamps via -ldflags -X.
package config

var (
	// CLIVersion is the released version, stamped by goreleaser.
	CLIVersion = "dev"
	// CLICommit is the short commit the binary was built from, stamped by goreleaser.
	CLICommit = "unknown"
)
