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
	"context"
	"os"
	"path/filepath"

	"github.com/zarf-dev/zarf/src/config"
	"github.com/zarf-dev/zarf/src/pkg/cluster"
	"github.com/zarf-dev/zarf/src/pkg/packager"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// RunRemove uninstalls the named package from the cluster KUBECONFIG points at - the removal
// Job's core (docs/dev/architecture.md §8). The package definition comes from zarf's own
// deployed-package record on that cluster, so a removal pulls nothing from any registry; a
// cluster with no record for the name has nothing to remove, which is success. Like Run, the
// outcome is reported as a Result - also on failure, so the controller always has a document
// to read.
func RunRemove(ctx context.Context, name, cachePath string) Result {
	result := Result{Name: name}

	// Removal actions stage through the same process-wide temp global as deploys; the pod's
	// root filesystem is read-only, so point it into the cache volume.
	tmp := filepath.Join(cachePath, "tmp")
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		result.Error = err.Error()
		return result
	}
	config.CommonOptions.TempDirectory = tmp

	c, err := cluster.New(ctx)
	if err != nil {
		result.Error = err.Error()
		return result
	}
	return removeFrom(ctx, c, name)
}

// removeFrom is RunRemove against an already-connected cluster, split so the record-reading
// branches are testable with a fake clientset.
func removeFrom(ctx context.Context, c *cluster.Cluster, name string) Result {
	result := Result{Name: name}
	deployed, err := c.GetDeployedPackage(ctx, name)
	if apierrors.IsNotFound(err) {
		result.Succeeded = true
		return result
	}
	if err != nil {
		result.Error = err.Error()
		return result
	}
	definition, err := deployed.Definition()
	if err != nil {
		result.Error = err.Error()
		return result
	}
	result.Version = definition.Metadata.Version

	if err := packager.Remove(ctx, definition, packager.RemoveOptions{Cluster: c}); err != nil {
		result.Error = err.Error()
		return result
	}
	result.Succeeded = true
	return result
}
