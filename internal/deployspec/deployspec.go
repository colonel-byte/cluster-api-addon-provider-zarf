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

// Package deployspec defines the rendered deploy spec - the deploy pod's entire input, written
// by the controller into a per-release Secret and mounted into the Job (§6 step 6: the pod is a
// pure operator, files in, termination message out). The deploy subcommand (M3) assembles
// packager options from this document alone; it holds no management-cluster reference, because
// the pod must need none.
package deployspec

import (
	"bytes"
	"fmt"
	"strings"
	"text/template"

	"sigs.k8s.io/yaml"
)

// FileName is the Secret key and the mounted file the deploy subcommand reads.
const FileName = "deploy-spec.yaml"

// InitFileName is the Secret key an auto-init Job's spec sits under, beside FileName in the same
// deploy-spec Secret - the same document shape, naming the init package (§6 step 5).
const InitFileName = "init-spec.yaml"

// DockerConfigFileName is the Secret key the registry credentials sit under, beside FileName.
// Literally config.json, because the mount directory becomes the pod's $DOCKER_CONFIG and that
// is the only name a Docker config directory holds its config under.
const DockerConfigFileName = "config.json"

// CAFileName is the Secret key the registry CA bundle sits under, beside FileName.
const CAFileName = "ca.crt"

// Version is this document's schema version, bumped on incompatible change - the controller
// and the deploy subcommand ship in one image, but a Job stamped before an upgrade can run
// after it. Under the project's one identity domain, same as the API group - this is a file
// format, not a Kubernetes API, but it gets no second namespace for being one.
const Version = "addons.colonel-byte.dev/deployspec/v1"

// Component selects one package component, optionally at a flavor.
type Component struct {
	Name   string `json:"name"`
	Flavor string `json:"flavor,omitempty"`
}

// Spec is the document. Package carries the digest-pinned reference - `repository@digest`,
// never a tag - which is the §6 enforcement point: what was verified at resolution and what
// the Job pulls are the same bytes by content addressing.
type Spec struct {
	Version    string            `json:"version"`
	Package    string            `json:"package"`
	PackageAPI string            `json:"packageAPI,omitempty"`
	Components []Component       `json:"components,omitempty"`
	Values     string            `json:"values,omitempty"`
	Set        map[string]string `json:"set,omitempty"`

	// PlainHTTP carries the manager's --registry-plain-http flag to the pod, so the Job pulls
	// over the same scheme the controller resolved and verified over. Test topologies only.
	PlainHTTP bool `json:"plainHTTP,omitempty"`

	// InsecureSkipTLSVerify carries the source's field of the same name. The credentials and
	// the CA travel as mounted files rather than fields - see DockerConfigFileName.
	InsecureSkipTLSVerify bool `json:"insecureSkipTLSVerify,omitempty"`
}

// Marshal renders the document as the YAML the Secret carries.
func (s Spec) Marshal() ([]byte, error) {
	if !strings.Contains(s.Package, "@sha256:") {
		// The one invariant worth a hard stop: a tag here would re-open the re-push window
		// verification closed.
		return nil, fmt.Errorf("deploy spec package %q is not digest-pinned", s.Package)
	}
	s.Version = Version
	return yaml.Marshal(s)
}

// Render substitutes `{{ .Cluster... }}` expressions in src against the target cluster's
// object content. missingkey errors rather than writing "<no value>" into a deploy: a typo in
// a template is a configuration error to surface, not a string to ship.
func Render(src string, cluster map[string]any) (string, error) {
	if !strings.Contains(src, "{{") {
		return src, nil
	}
	tmpl, err := template.New(FileName).Option("missingkey=error").Parse(src)
	if err != nil {
		return "", fmt.Errorf("parsing the value template: %w", err)
	}
	var out bytes.Buffer
	if err := tmpl.Execute(&out, map[string]any{"Cluster": cluster}); err != nil {
		return "", fmt.Errorf("rendering the value template: %w", err)
	}
	return out.String(), nil
}
