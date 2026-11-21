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

package release

import "testing"

// TestZarfVersionPattern pins the extraction against the exact line shape zarf.yaml carries,
// including the x-release-please trailing comment it must not swallow.
func TestZarfVersionPattern(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "version with the release-please comment",
			input: "metadata:\n  name: x\n  version: 0.1.0 # x-release-please-version\n",
			want:  "0.1.0",
		},
		{
			name:  "bare version",
			input: "metadata:\n  version: 1.2.3\n",
			want:  "1.2.3",
		},
		{
			name:  "no version line",
			input: "metadata:\n  name: x\n",
			want:  "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			match := zarfVersionPattern.FindStringSubmatch(tt.input)
			got := ""
			if match != nil {
				got = match[1]
			}
			if got != tt.want {
				t.Errorf("extracted %q, want %q", got, tt.want)
			}
		})
	}
}
