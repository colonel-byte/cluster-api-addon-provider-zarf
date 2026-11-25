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
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// managerContainerName is the container the deployment runs this binary as; its image is the one
// deploy Jobs inherit. Named rather than indexed so an injected sidecar cannot change the answer.
const managerContainerName = "manager"

// containerImage returns the image the named container runs, preferring the status over the spec:
// the status carries what the kubelet actually resolved, so a spec naming a tag reports the
// digest it pulled, and deploy Jobs then run the exact bytes this controller is running.
func containerImage(pod *corev1.Pod, container string) (string, error) {
	for _, status := range pod.Status.ContainerStatuses {
		if status.Name == container && status.ImageID != "" && status.Image != "" {
			return status.Image, nil
		}
	}
	for _, spec := range pod.Spec.Containers {
		if spec.Name == container {
			return spec.Image, nil
		}
	}
	return "", fmt.Errorf("pod %s/%s runs no container named %q", pod.Namespace, pod.Name, container)
}

// selfImage reads the image this controller's own pod runs, which is what deploy Jobs default to
// when --deploy-image names nothing (docs/dev/architecture.md §6 step 6: a deploy Job runs "the
// controller's own image"). The pod is located through the downward-API environment the manager
// deployment sets; without it there is nothing to read and the flag is the only answer.
func selfImage(ctx context.Context, c client.Reader, namespace, name string) (string, error) {
	if namespace == "" || name == "" {
		return "", fmt.Errorf("POD_NAMESPACE and POD_NAME are not both set, so this controller " +
			"cannot read its own image; pass --deploy-image")
	}
	pod := &corev1.Pod{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, pod); err != nil {
		return "", fmt.Errorf("reading this controller's own pod %s/%s: %w", namespace, name, err)
	}
	return containerImage(pod, managerContainerName)
}
