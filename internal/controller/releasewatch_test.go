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
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	addonsv1alpha1 "github.com/colonel-byte/cluster-api-addon-provider-zarf/api/v1alpha1"
)

func TestReleaseToProxy(t *testing.T) {
	tests := []struct {
		name          string
		refName       string
		refNamespace  string
		wantNamespace string
	}{
		{
			name:          "local ref resolves in the release's namespace",
			refName:       "security-baseline",
			refNamespace:  "",
			wantNamespace: "fleet-default",
		},
		{
			name:          "cross-namespace ref routes to the named namespace",
			refName:       "security-baseline",
			refNamespace:  "platform",
			wantNamespace: "platform",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			release := &addonsv1alpha1.ZarfPackageRelease{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "example-security-baseline",
					Namespace: "fleet-default",
				},
				Spec: addonsv1alpha1.ZarfPackageReleaseSpec{
					ClusterRef: corev1.LocalObjectReference{Name: "example-cluster"},
					PackageProxyRef: addonsv1alpha1.NamespacedReference{
						Name:      tt.refName,
						Namespace: tt.refNamespace,
					},
				},
			}
			reqs := releaseToProxy(context.Background(), release)
			if len(reqs) != 1 {
				t.Fatalf("releaseToProxy() returned %d requests, want 1", len(reqs))
			}
			if reqs[0].Namespace != tt.wantNamespace || reqs[0].Name != tt.refName {
				t.Errorf("releaseToProxy() = %s/%s, want %s/%s", reqs[0].Namespace, reqs[0].Name, tt.wantNamespace, tt.refName)
			}
		})
	}

	if reqs := releaseToProxy(context.Background(), &addonsv1alpha1.ZarfPackageProxy{}); reqs != nil {
		t.Errorf("releaseToProxy() on a non-release = %v, want nil", reqs)
	}
}
