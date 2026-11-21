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

package e2e

import (
	"strings"
	"testing"
)

// TestKindConfig checks the three properties the topology depends on: the pinned node image,
// the docker socket mount CAPD requires, and the containerd config_path that makes per-registry
// hosts.toml files effective.
func TestKindConfig(t *testing.T) {
	config := kindConfig()
	tests := []struct {
		name string
		want string
	}{
		{
			name: "pinned node image by digest",
			want: kindNodeImage,
		},
		{
			name: "docker socket mount for CAPD",
			want: "/var/run/docker.sock",
		},
		{
			name: "containerd certs.d config path",
			want: `config_path = "/etc/containerd/certs.d"`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !strings.Contains(config, tt.want) {
				t.Errorf("kind config is missing %q:\n%s", tt.want, config)
			}
		})
	}
}

// TestHostsToml checks the registry is declared as a plain-HTTP pull endpoint under its one
// stable name.
func TestHostsToml(t *testing.T) {
	toml := hostsToml()
	if !strings.Contains(toml, `[host."http://`+registryHost+`"]`) {
		t.Errorf("hosts.toml does not declare the HTTP endpoint:\n%s", toml)
	}
	if strings.Contains(toml, "\"") && !strings.Contains(toml, "pull") {
		t.Errorf("hosts.toml grants no pull capability:\n%s", toml)
	}
}
