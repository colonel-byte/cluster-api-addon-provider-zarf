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

// Package deploy is the deploy subcommand's core - the code that runs inside a deploy Job's pod
// (docs/dev/architecture.md §6 step 6). The pod is a pure operator: its entire input is the
// mounted deploy-spec file, the kubeconfig the environment points at, and the cache volume; its
// entire output is the termination-message Result. Nothing here may reach the management
// cluster.
package deploy

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/zarf-dev/zarf/src/config"
	"github.com/zarf-dev/zarf/src/pkg/packager"
	"github.com/zarf-dev/zarf/src/pkg/packager/filters"
	"github.com/zarf-dev/zarf/src/pkg/packager/layout"
	"github.com/zarf-dev/zarf/src/pkg/state"
	"github.com/zarf-dev/zarf/src/types"
	"sigs.k8s.io/yaml"

	"github.com/colonel-byte/cluster-api-addon-provider-zarf/internal/deployspec"
)

// Result is the termination-message document the controller reads back from the finished pod -
// the pod's only output channel. Kept small: a container's termination message is capped at
// 4096 bytes.
type Result struct {
	// Succeeded is the one bit the controller gates the release phase on.
	Succeeded bool `json:"succeeded"`

	// Package is the digest-pinned reference the spec named - the source of
	// status.deployedDigest, read from the pod that ran rather than the spec the controller
	// currently holds, so a Job from an older rendering reports what it actually deployed.
	Package string `json:"package,omitempty"`

	// Name is the zarf package's declared name - the identity the deployed-package record on
	// the workload cluster is keyed on (§3).
	Name string `json:"name,omitempty"`

	// Version is the package's declared metadata version, for status.deployedVersion.
	Version string `json:"version,omitempty"`

	// Error carries the failure, truncated to fit the termination message.
	Error string `json:"error,omitempty"`
}

// Load reads and validates the mounted deploy-spec file. Validation here is a trust boundary in
// time, not in actor - the controller wrote the file, but possibly from an older or newer image
// than the one running this code.
func Load(path string) (deployspec.Spec, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return deployspec.Spec{}, err
	}
	var spec deployspec.Spec
	if err := yaml.UnmarshalStrict(raw, &spec); err != nil {
		return deployspec.Spec{}, fmt.Errorf("parsing the deploy spec: %w", err)
	}
	if spec.Version != deployspec.Version {
		return deployspec.Spec{}, fmt.Errorf("deploy spec version %q is not %q; the controller and this image disagree", spec.Version, deployspec.Version)
	}
	if !strings.Contains(spec.Package, "@sha256:") {
		// Same invariant Marshal enforces on the way in, re-checked on the way out: a tag here
		// would re-open the re-push window verification closed (§6).
		return deployspec.Spec{}, fmt.Errorf("deploy spec package %q is not digest-pinned", spec.Package)
	}
	for _, component := range spec.Components {
		if component.Flavor != "" {
			// shortcut: zarf v1.0.1 has no deploy-time flavor selection - flavor is baked in at
			// package create - so a requested flavor cannot be honored; refuse rather than
			// silently deploy the wrong variant. Revisit if upstream grows a deploy-time filter.
			return deployspec.Spec{}, fmt.Errorf("component %q requests flavor %q, but zarf selects flavors at package create time, not deploy time", component.Name, component.Flavor)
		}
	}
	return spec, nil
}

// Options assembles the SDK options from the spec. Split from Run so the mapping is testable
// without a registry or cluster.
func Options(spec deployspec.Spec, cachePath string) (packager.LoadOptions, packager.DeployOptions, error) {
	loadOpts := packager.LoadOptions{
		CachePath: cachePath,
		// The controller verified the signature at resolution; the digest makes the pulled
		// bytes the verified bytes, so the pod re-verifies nothing and needs no key.
		VerificationStrategy: layout.VerifyNever,
		// shortcut: amd64 mirrors the resolver's default rather than the pod's own arch - the
		// package platform must match the target cluster, not this pod; widen with §4D's
		// architecture detection.
		Architecture:  "amd64",
		RemoteOptions: types.RemoteOptions{PlainHTTP: spec.PlainHTTP},
	}
	// Always the deployment filter, even with nothing selected: it is what "empty means the
	// package's defaults" (the spec.components doc) actually requires. With no filter at all a
	// package deploys every component it carries, optional ones included - on the Zarf init
	// package that means its k3s component, which installs k3s on the node nobody asked to
	// have reprovisioned. ForDeploy is the CLI's own non-interactive selection: required
	// components, plus any named here, honoring v1alpha1 defaults and choice groups.
	selectors := make([]string, 0, len(spec.Components))
	for _, component := range spec.Components {
		selectors = append(selectors, component.Name)
	}
	loadOpts.Filter = filters.ForDeploy(strings.Join(selectors, ","))

	deployOpts := packager.DeployOptions{
		// Air-gap is this provider's whole premise; YOLO packages flip themselves to
		// connected inside Deploy.
		Connectivity:  state.PackageConnectivityAirGap,
		SetVariables:  spec.Set,
		RemoteOptions: types.RemoteOptions{PlainHTTP: spec.PlainHTTP},
	}
	if spec.Values != "" {
		if err := yaml.Unmarshal([]byte(spec.Values), &deployOpts.Values); err != nil {
			return packager.LoadOptions{}, packager.DeployOptions{}, fmt.Errorf("parsing the rendered values: %w", err)
		}
	}
	return loadOpts, deployOpts, nil
}

// Run deploys per the spec and reports the outcome as a Result - also on failure, so the
// controller always has a document to read.
func Run(ctx context.Context, spec deployspec.Spec, cachePath string) Result {
	result := Result{Package: spec.Package}

	// zarf stages temp workspaces via this process-wide global; the pod's root filesystem is
	// read-only, so point it into the cache volume.
	tmp := filepath.Join(cachePath, "tmp")
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		result.Error = err.Error()
		return result
	}
	config.CommonOptions.TempDirectory = tmp

	loadOpts, deployOpts, err := Options(spec, cachePath)
	if err != nil {
		result.Error = err.Error()
		return result
	}
	pkgLayout, err := packager.LoadPackage(ctx, "oci://"+spec.Package, loadOpts)
	if err != nil {
		result.Error = fmt.Sprintf("loading %s: %s", spec.Package, err)
		return result
	}
	definition := pkgLayout.Definition()
	result.Name = definition.Metadata.Name
	result.Version = definition.Metadata.Version

	if _, err := packager.Deploy(ctx, pkgLayout, deployOpts); err != nil {
		result.Error = err.Error()
		return result
	}
	result.Succeeded = true
	return result
}

// terminationMessageLimit is Kubernetes' per-container cap; a longer write is truncated by the
// kubelet from the front, which would cut the fields rather than the error tail.
const terminationMessageLimit = 4096

// WriteResult writes the result where the kubelet reads the termination message. The error is
// truncated first so the document always fits whole.
func WriteResult(path string, result Result) error {
	raw, err := yaml.Marshal(result)
	if err != nil {
		return err
	}
	if overflow := len(raw) - terminationMessageLimit; overflow > 0 {
		// Cut past the overflow by the appended marker's length; shortening the string can
		// only shrink its escaped form, so the remarshal fits.
		keep := len(result.Error) - overflow - 3
		if keep < 0 {
			keep = 0
		}
		result.Error = result.Error[:keep] + "..."
		if raw, err = yaml.Marshal(result); err != nil {
			return err
		}
	}
	return os.WriteFile(path, raw, 0o644)
}
