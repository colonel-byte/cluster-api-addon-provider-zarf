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
	"strings"
	"testing"

	"github.com/zarf-dev/zarf/src/pkg/cluster"
	"github.com/zarf-dev/zarf/src/pkg/state"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestRemoveFrom(t *testing.T) {
	tests := []struct {
		name          string
		secret        *corev1.Secret
		wantSucceeded bool
		wantError     string
	}{
		{
			// §8's quiet success: no deployed-package record means nothing to remove - a
			// release deleted before its package ever deployed must still finalize.
			name:          "no deployed-package record succeeds",
			secret:        nil,
			wantSucceeded: true,
		},
		{
			name: "an unreadable record fails rather than orphaning silently",
			secret: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: state.ZarfNamespaceName,
					Name:      "zarf-package-podinfo",
				},
				Data: map[string][]byte{"data": []byte("not a package record")},
			},
			wantSucceeded: false,
			wantError:     "unable to decode",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clientset := fake.NewClientset()
			if tt.secret != nil {
				clientset = fake.NewClientset(tt.secret)
			}
			c := &cluster.Cluster{Clientset: clientset}

			result := removeFrom(t.Context(), c, "podinfo")
			if result.Succeeded != tt.wantSucceeded {
				t.Fatalf("Succeeded = %v, want %v (error %q)", result.Succeeded, tt.wantSucceeded, result.Error)
			}
			if !strings.Contains(result.Error, tt.wantError) {
				t.Fatalf("Error = %q, want it to contain %q", result.Error, tt.wantError)
			}
			if result.Name != "podinfo" {
				t.Fatalf("Name = %q, want podinfo", result.Name)
			}
		})
	}
}
