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

package main

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestContainerImage covers what deploy Jobs inherit: the resolved image from the status where
// the kubelet reported one, the spec otherwise, and an error when no container matches - the
// case that used to produce an empty image and a Job the API server refused.
func TestContainerImage(t *testing.T) {
	cases := []struct {
		name    string
		pod     *corev1.Pod
		want    string
		wantErr bool
	}{
		{
			name: "status wins, so a tag resolves to the digest actually pulled",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:  "manager",
						Image: "registry.internal/caapz:v1",
					}},
				},
				Status: corev1.PodStatus{
					ContainerStatuses: []corev1.ContainerStatus{{
						Name:    "manager",
						Image:   "registry.internal/caapz@sha256:abcd",
						ImageID: "docker-pullable://registry.internal/caapz@sha256:abcd",
					}},
				},
			},
			want: "registry.internal/caapz@sha256:abcd",
		},
		{
			name: "spec is the fallback before the kubelet has reported",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:  "manager",
						Image: "registry.internal/caapz:v1",
					}},
				},
			},
			want: "registry.internal/caapz:v1",
		},
		{
			name: "a sidecar's image is never mistaken for the manager's",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "sidecar",
							Image: "registry.internal/proxy:v9",
						},
						{
							Name:  "manager",
							Image: "registry.internal/caapz:v1",
						},
					},
				},
			},
			want: "registry.internal/caapz:v1",
		},
		{
			name: "no manager container is an error, not an empty image",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Namespace: "caapz-system", Name: "pod"},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:  "sidecar",
						Image: "registry.internal/proxy:v9",
					}},
				},
			},
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := containerImage(tc.pod, managerContainerName)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("containerImage = %q, want an error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("containerImage: %v", err)
			}
			if got != tc.want {
				t.Fatalf("containerImage = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestSelfImageWithoutDownwardAPI pins the refusal: with no pod identity in the environment
// there is nothing to read, and the error has to name the flag that resolves it rather than
// letting an empty image reach a Job.
func TestSelfImageWithoutDownwardAPI(t *testing.T) {
	if _, err := selfImage(t.Context(), nil, "", ""); err == nil {
		t.Fatal("selfImage with no POD_NAMESPACE or POD_NAME returned no error")
	}
}
