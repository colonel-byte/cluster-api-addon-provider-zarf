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

package controller

import (
	"slices"
	"testing"

	addonsv1alpha1 "github.com/colonel-byte/cluster-api-addon-provider-zarf/api/v1alpha1"
)

func TestProxyDependencies(t *testing.T) {
	tests := []struct {
		name      string
		dependsOn []addonsv1alpha1.DependencyReference
		want      []string
	}{
		{
			name: "an empty kind is a proxy, so a bare name still works",
			dependsOn: []addonsv1alpha1.DependencyReference{
				{
					Name: "baseline",
				},
			},
			want: []string{"baseline"},
		},
		{
			name: "an explicit proxy kind",
			dependsOn: []addonsv1alpha1.DependencyReference{
				{
					Kind: addonsv1alpha1.DependencyKindPackageProxy,
					Name: "baseline",
				},
			},
			want: []string{"baseline"},
		},
		{
			name: "a kind this controller does not resolve is not a proxy edge",
			dependsOn: []addonsv1alpha1.DependencyReference{
				{
					Kind: "ZarfPackageBundle",
					Name: "bootstrap",
				},
				{
					Name: "baseline",
				},
			},
			// Only the proxy edge: the cycle walk and the watch fan-out must not follow a
			// name through the wrong kind.
			want: []string{"baseline"},
		},
		{
			name:      "nothing declared",
			dependsOn: nil,
			want:      []string{},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			proxy := &addonsv1alpha1.ZarfPackageProxy{}
			proxy.Spec.DependsOn = test.dependsOn
			got := proxyDependencies(proxy)
			if !slices.Equal(got, test.want) {
				t.Fatalf("want %v, got %v", test.want, got)
			}
		})
	}
}

func TestDependencyReferenceProxyKind(t *testing.T) {
	tests := []struct {
		name string
		kind addonsv1alpha1.DependencyKind
		want bool
	}{
		{
			name: "empty defaults to a proxy",
			kind: "",
			want: true,
		},
		{
			name: "the proxy kind",
			kind: addonsv1alpha1.DependencyKindPackageProxy,
			want: true,
		},
		{
			name: "any other kind",
			kind: "ZarfPackageBundle",
			want: false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ref := addonsv1alpha1.DependencyReference{Kind: test.kind, Name: "x"}
			if got := ref.ProxyKind(); got != test.want {
				t.Fatalf("want %t, got %t", test.want, got)
			}
		})
	}
}
