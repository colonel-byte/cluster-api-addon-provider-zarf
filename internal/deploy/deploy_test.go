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

package deploy

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/zarf-dev/zarf/src/api"
	"sigs.k8s.io/yaml"

	"github.com/colonel-byte/cluster-api-addon-provider-zarf/internal/deployspec"
)

func writeSpec(t *testing.T, spec deployspec.Spec) string {
	t.Helper()
	raw, err := spec.Marshal()
	if err != nil {
		t.Fatalf("marshaling the spec: %v", err)
	}
	path := filepath.Join(t.TempDir(), deployspec.FileName)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("writing the spec: %v", err)
	}
	return path
}

func TestLoad(t *testing.T) {
	good := deployspec.Spec{
		Package: "registry.local/pkg@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
	}

	t.Run("round-trips a marshaled spec", func(t *testing.T) {
		spec, err := Load(writeSpec(t, good))
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if spec.Package != good.Package {
			t.Fatalf("package = %q, want %q", spec.Package, good.Package)
		}
	})

	t.Run("refuses a version this image does not speak", func(t *testing.T) {
		raw, err := good.Marshal()
		if err != nil {
			t.Fatal(err)
		}
		mangled := strings.Replace(string(raw), deployspec.Version, "addons.colonel-byte.dev/deployspec/v999", 1)
		path := filepath.Join(t.TempDir(), deployspec.FileName)
		if err := os.WriteFile(path, []byte(mangled), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "deploy spec version") {
			t.Fatalf("want a version mismatch error, got %v", err)
		}
	})

	t.Run("refuses a tag reference", func(t *testing.T) {
		raw, err := yaml.Marshal(deployspec.Spec{Version: deployspec.Version, Package: "registry.local/pkg:v1"})
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(t.TempDir(), deployspec.FileName)
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "not digest-pinned") {
			t.Fatalf("want a digest-pin error, got %v", err)
		}
	})

	t.Run("refuses a component flavor", func(t *testing.T) {
		flavored := good
		flavored.Components = []deployspec.Component{
			{
				Name:   "core",
				Flavor: "vsphere",
			},
		}
		if _, err := Load(writeSpec(t, flavored)); err == nil || !strings.Contains(err.Error(), "flavor") {
			t.Fatalf("want a flavor refusal, got %v", err)
		}
	})
}

func TestOptions(t *testing.T) {
	spec := deployspec.Spec{
		Package: "registry.local/pkg@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		Components: []deployspec.Component{
			{
				Name: "core",
			},
		},
		Values:    "log:\n  level: debug\n",
		Set:       map[string]string{"DOMAIN": "example.com"},
		PlainHTTP: true,
	}

	loadOpts, deployOpts, err := Options(spec, "/cache")
	if err != nil {
		t.Fatalf("Options: %v", err)
	}
	if loadOpts.CachePath != "/cache" {
		t.Errorf("cache path = %q", loadOpts.CachePath)
	}
	if !loadOpts.PlainHTTP || !deployOpts.PlainHTTP {
		t.Error("PlainHTTP did not reach both option sets")
	}
	if loadOpts.Filter == nil {
		t.Error("components did not become a filter")
	}
	if deployOpts.SetVariables["DOMAIN"] != "example.com" {
		t.Errorf("set variables = %v", deployOpts.SetVariables)
	}
	level, ok := deployOpts.Values["log"].(map[string]any)
	if !ok || level["level"] != "debug" {
		t.Errorf("values = %v", deployOpts.Values)
	}

	bad := spec
	bad.Values = "::not yaml"
	if _, _, err := Options(bad, "/cache"); err == nil {
		t.Error("want an error for unparsable values")
	}
}

// TestOptionsComponentFilter is the behaviour the filter exists for: an optional component -
// the Zarf init package's k3s, which reprovisions the node - stays out of a deploy unless it is
// named, and required components come along either way. Before this filter was applied
// unconditionally, an empty spec.components deployed every component the package carried.
func TestOptionsComponentFilter(t *testing.T) {
	pkg := api.Package{
		Components: []api.Component{
			{
				Name: "required-core",
			},
			{
				Name:     "optional-k3s",
				Optional: true,
			},
		},
	}

	cases := []struct {
		name       string
		components []deployspec.Component
		want       []string
	}{
		{
			name:       "nothing selected takes the package's defaults",
			components: nil,
			want:       []string{"required-core"},
		},
		{
			name: "naming the optional one opts into it",
			components: []deployspec.Component{
				{
					Name: "optional-k3s",
				},
			},
			want: []string{"required-core", "optional-k3s"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := deployspec.Spec{
				Package:    "registry.local/pkg@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
				Components: tc.components,
			}
			loadOpts, _, err := Options(spec, "/cache")
			if err != nil {
				t.Fatalf("Options: %v", err)
			}
			if loadOpts.Filter == nil {
				t.Fatal("Options left no component filter, so every component would deploy")
			}
			selected, err := loadOpts.Filter.Apply(pkg)
			if err != nil {
				t.Fatalf("applying the filter: %v", err)
			}
			got := make([]string, 0, len(selected))
			for _, component := range selected {
				got = append(got, component.Name)
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("selected components = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestWriteResult(t *testing.T) {
	t.Run("writes the document", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "termination-log")
		result := Result{
			Succeeded: true,
			Package:   "registry.local/pkg@sha256:abc",
			Name:      "pkg",
			Version:   "1.0.0",
		}
		if err := WriteResult(path, result); err != nil {
			t.Fatalf("WriteResult: %v", err)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var read Result
		if err := yaml.Unmarshal(raw, &read); err != nil {
			t.Fatalf("unmarshaling: %v", err)
		}
		if read != result {
			t.Fatalf("round trip = %+v, want %+v", read, result)
		}
	})

	t.Run("truncates an oversized error to fit", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "termination-log")
		result := Result{
			Package: "registry.local/pkg@sha256:abc",
			Error:   strings.Repeat("x", 8000),
		}
		if err := WriteResult(path, result); err != nil {
			t.Fatalf("WriteResult: %v", err)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if len(raw) > terminationMessageLimit {
			t.Fatalf("wrote %d bytes, limit is %d", len(raw), terminationMessageLimit)
		}
		var read Result
		if err := yaml.Unmarshal(raw, &read); err != nil {
			t.Fatalf("the truncated document no longer parses: %v", err)
		}
		if read.Package != result.Package || !strings.HasSuffix(read.Error, "...") {
			t.Fatalf("truncation lost fields: %+v", read)
		}
	})
}
