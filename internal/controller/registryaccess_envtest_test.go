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

//go:build envtest

package controller

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	addonsv1alpha1 "github.com/colonel-byte/cluster-api-addon-provider-zarf/api/v1alpha1"
	"github.com/colonel-byte/cluster-api-addon-provider-zarf/internal/envtest"
)

// TestRegistryAccessValidation runs the package source's two registry CEL rules against the
// real API server. The rules' `has()` guards are the part worth a test rather than a reading:
// a rule that dereferences an unset optional boolean fails *every* create, including the ones
// it was meant to allow, and nothing but an API server says so.
func TestRegistryAccessValidation(t *testing.T) {
	c := envtest.Client(t)
	ns := envtest.Namespace(t, c)

	tests := []struct {
		name    string
		source  addonsv1alpha1.ZarfPackageSource
		refused string
	}{
		{
			name: "nothing named",
			source: addonsv1alpha1.ZarfPackageSource{
				Repository: "registry.internal/zarf/podinfo",
				Version:    "6.5.0",
			},
		},
		{
			name: "credentials and a CA from a secret",
			source: addonsv1alpha1.ZarfPackageSource{
				Repository:        "registry.internal/zarf/podinfo",
				Version:           "6.5.0",
				PullSecretRef:     &addonsv1alpha1.NamespacedReference{Name: "registry-creds"},
				CABundleSecretRef: &addonsv1alpha1.NamespacedReference{Name: "registry-ca"},
			},
		},
		{
			name: "a CA from a configmap",
			source: addonsv1alpha1.ZarfPackageSource{
				Repository:           "registry.internal/zarf/podinfo",
				Version:              "6.5.0",
				CABundleConfigMapRef: &addonsv1alpha1.NamespacedReference{Name: "registry-ca"},
			},
		},
		{
			name: "insecureSkipTLSVerify alone",
			source: addonsv1alpha1.ZarfPackageSource{
				Repository:            "registry.internal/zarf/podinfo",
				Version:               "6.5.0",
				InsecureSkipTLSVerify: true,
			},
		},
		{
			name: "fromZarfState alone",
			source: addonsv1alpha1.ZarfPackageSource{
				Repository:    "registry.internal/zarf/podinfo",
				Version:       "6.5.0",
				FromZarfState: true,
			},
		},
		{
			name: "a CA from both a secret and a configmap",
			source: addonsv1alpha1.ZarfPackageSource{
				Repository:           "registry.internal/zarf/podinfo",
				Version:              "6.5.0",
				CABundleSecretRef:    &addonsv1alpha1.NamespacedReference{Name: "registry-ca"},
				CABundleConfigMapRef: &addonsv1alpha1.NamespacedReference{Name: "registry-ca"},
			},
			refused: "not both",
		},
		{
			name: "fromZarfState beside credentials it would contradict",
			source: addonsv1alpha1.ZarfPackageSource{
				Repository:    "registry.internal/zarf/podinfo",
				Version:       "6.5.0",
				FromZarfState: true,
				PullSecretRef: &addonsv1alpha1.NamespacedReference{Name: "registry-creds"},
			},
			refused: "fromZarfState supplies",
		},
		{
			name: "fromZarfState beside a CA it would contradict",
			source: addonsv1alpha1.ZarfPackageSource{
				Repository:           "registry.internal/zarf/podinfo",
				Version:              "6.5.0",
				FromZarfState:        true,
				CABundleConfigMapRef: &addonsv1alpha1.NamespacedReference{Name: "registry-ca"},
			},
			refused: "fromZarfState supplies",
		},
	}

	for i, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			// The same source shape is validated wherever it appears, so each case is checked
			// on spec.package and on spec.init.package - the second is the one a rule written
			// against the top-level path would silently miss.
			proxy := &addonsv1alpha1.ZarfPackageProxy{
				ObjectMeta: metav1.ObjectMeta{Name: "registry-access-" + itoa(i), Namespace: ns},
				Spec: addonsv1alpha1.ZarfPackageProxySpec{
					ClusterSelector: metav1.LabelSelector{},
					Package:         &test.source,
					Init:            &addonsv1alpha1.InitOptions{Package: test.source.DeepCopy()},
				},
			}
			err := c.Create(t.Context(), proxy)
			if test.refused == "" {
				if err != nil {
					t.Fatalf("want the source accepted, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("want the source refused, got no error")
			}
			if !strings.Contains(err.Error(), test.refused) {
				t.Fatalf("want a refusal mentioning %q, got %v", test.refused, err)
			}
		})
	}
}

func itoa(i int) string {
	return string(rune('a' + i))
}
