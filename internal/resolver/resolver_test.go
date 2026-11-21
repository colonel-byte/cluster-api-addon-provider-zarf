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

package resolver

import "testing"

// TestSchemaVersion pins the reduction from a package apiVersion to the bare enum value
// spec.package.api speaks, including the legacy empty form Resolve maps before calling it.
func TestSchemaVersion(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "v1beta1 with the group",
			input: "zarf.dev/v1beta1",
			want:  "v1beta1",
		},
		{
			name:  "v1alpha1 with the group",
			input: "zarf.dev/v1alpha1",
			want:  "v1alpha1",
		},
		{
			name:  "bare version passes through",
			input: "v1beta1",
			want:  "v1beta1",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := schemaVersion(tt.input); got != tt.want {
				t.Errorf("schemaVersion(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}
