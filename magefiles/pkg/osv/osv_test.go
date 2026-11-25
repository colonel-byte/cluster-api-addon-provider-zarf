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

package osv_test

import (
	"strings"
	"testing"

	"github.com/colonel-byte/cluster-api-addon-provider-zarf/magefiles/pkg/osv"
)

func TestForeignManifest(t *testing.T) {
	tests := []struct {
		name     string
		filename string
		want     bool
	}{
		{
			name:     "npm lockfile",
			filename: "package-lock.json",
			want:     true,
		},
		{
			name:     "yarn lockfile",
			filename: "yarn.lock",
			want:     true,
		},
		{
			name:     "plain python requirements",
			filename: "requirements.txt",
			want:     true,
		},
		{
			name:     "suffixed python requirements",
			filename: "requirements-test.txt",
			want:     true,
		},
		{
			name:     "maven pom",
			filename: "pom.xml",
			want:     true,
		},
		{
			name:     "go module file",
			filename: "go.mod",
			want:     false,
		},
		{
			name:     "go checksum file",
			filename: "go.sum",
			want:     false,
		},
		{
			name:     "go source",
			filename: "main.go",
			want:     false,
		},
		{
			name:     "unrelated text file",
			filename: "test.txt",
			want:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := osv.ForeignManifest(tt.filename); got != tt.want {
				t.Errorf("ForeignManifest(%q) = %v, want %v", tt.filename, got, tt.want)
			}
		})
	}
}

func TestOverrideContent(t *testing.T) {
	content := osv.OverrideContent(osv.Override{
		Dir:       "vendor/example.com/foo",
		Manifest:  "package-lock.json",
		Ecosystem: "npm",
		What:      "the dev dependencies",
	})

	for _, want := range []string{
		`ecosystem = "npm"`,
		"ignore = true",
		"JavaScript tooling vendored alongside a Go module",
		"package-lock.json here is the dev dependencies.",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("OverrideContent() is missing %q, got:\n%s", want, content)
		}
	}

	// The header is a hand-rolled word wrapper, so check it produced comments rather than
	// overlong lines or a stray bare word.
	for _, line := range strings.Split(strings.TrimSpace(content), "\n") {
		if !strings.HasPrefix(line, "#") {
			break // the TOML body starts here
		}
		if len(line) > 98 {
			t.Errorf("comment line is %d chars, want <= 98: %q", len(line), line)
		}
	}
}
