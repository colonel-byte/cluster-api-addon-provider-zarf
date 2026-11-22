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

package deployspec

import (
	"strings"
	"testing"
)

// TestRender pins the substitution contract: cluster fields resolve, template-free input passes
// through untouched, and a missing field is an error rather than "<no value>" in a deploy.
func TestRender(t *testing.T) {
	cluster := map[string]any{
		"metadata": map[string]any{
			"name": "edge-1",
			"labels": map[string]any{
				"tier": "edge",
			},
		},
	}
	tests := []struct {
		name    string
		src     string
		want    string
		wantErr bool
	}{
		{
			name: "metadata name resolves",
			src:  "cluster: {{ .Cluster.metadata.name }}",
			want: "cluster: edge-1",
		},
		{
			name: "label lookup resolves",
			src:  `tier: {{ index .Cluster.metadata.labels "tier" }}`,
			want: "tier: edge",
		},
		{
			name: "template-free input passes through",
			src:  "plain: value",
			want: "plain: value",
		},
		{
			name:    "missing field errors",
			src:     "{{ .Cluster.metadata.nope }}",
			wantErr: true,
		},
		{
			name:    "parse error surfaces",
			src:     "{{ .Cluster.metadata.name",
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Render(tt.src, cluster)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Render(%q) succeeded with %q, want an error", tt.src, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("Render(%q): %v", tt.src, err)
			}
			if got != tt.want {
				t.Errorf("Render(%q) = %q, want %q", tt.src, got, tt.want)
			}
		})
	}
}

// TestMarshalRefusesTags pins the one hard invariant: a deploy spec never carries a tag.
func TestMarshalRefusesTags(t *testing.T) {
	if _, err := (Spec{Package: "registry.internal/zarf/app:1.0.0"}).Marshal(); err == nil {
		t.Fatal("a tag reference marshaled; the deploy spec must be digest-pinned")
	}
	out, err := (Spec{Package: "registry.internal/zarf/app@sha256:abc"}).Marshal()
	if err != nil {
		t.Fatalf("a digest reference refused: %v", err)
	}
	if !strings.Contains(string(out), Version) {
		t.Fatalf("marshaled spec carries no schema version:\n%s", out)
	}
}
