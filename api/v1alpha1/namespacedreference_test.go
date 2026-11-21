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

package v1alpha1

import (
	"testing"
)

func TestNamespacedReferenceKey(t *testing.T) {
	tests := []struct {
		name    string
		ref     NamespacedReference
		allowed []string
		wantNS  string
		wantErr bool
	}{
		{
			name:    "empty namespace resolves to owner",
			ref:     NamespacedReference{Name: "key"},
			allowed: nil,
			wantNS:  "fleet-default",
			wantErr: false,
		},
		{
			name:    "own namespace spelled out",
			ref:     NamespacedReference{Name: "key", Namespace: "fleet-default"},
			allowed: nil,
			wantNS:  "fleet-default",
			wantErr: false,
		},
		{
			name:    "cross-namespace refused when not allowed",
			ref:     NamespacedReference{Name: "key", Namespace: "other"},
			allowed: nil,
			wantNS:  "",
			wantErr: true,
		},
		{
			name:    "cross-namespace permitted by list",
			ref:     NamespacedReference{Name: "key", Namespace: "other"},
			allowed: []string{"other"},
			wantNS:  "other",
			wantErr: false,
		},
		{
			name:    "cross-namespace permitted by wildcard",
			ref:     NamespacedReference{Name: "key", Namespace: "other"},
			allowed: []string{"*"},
			wantNS:  "other",
			wantErr: false,
		},
		{
			name:    "listed namespace does not permit a different one",
			ref:     NamespacedReference{Name: "key", Namespace: "other"},
			allowed: []string{"third"},
			wantNS:  "",
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.ref.Key("fleet-default", tt.allowed, "--secret-namespaces")
			if (err != nil) != tt.wantErr {
				t.Fatalf("Key() error = %v, wantErr %v", err, tt.wantErr)
			}
			if got.Namespace != tt.wantNS {
				t.Errorf("Key() namespace = %q, want %q", got.Namespace, tt.wantNS)
			}
			if !tt.wantErr && got.Name != "key" {
				t.Errorf("Key() name = %q, want %q", got.Name, "key")
			}
		})
	}
}
