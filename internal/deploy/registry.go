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
	"crypto/x509"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/colonel-byte/cluster-api-addon-provider-zarf/internal/deployspec"
)

// systemCertFiles are the CA bundles crypto/x509 looks for on Linux, in its order, and the
// bundle this process' own image ships is one of them. Mirrored rather than imported because
// the list is unexported; wrong only if it is edited upstream, and then only by trusting the
// mounted CA alone - which is still a working deploy.
var systemCertFiles = []string{
	"/etc/ssl/certs/ca-certificates.crt",
	"/etc/pki/tls/certs/ca-bundle.crt",
	"/etc/ssl/ca-bundle.pem",
	"/etc/pki/tls/cacert.pem",
	"/etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem",
	"/etc/ssl/cert.pem",
}

// Reachability applies the registry credentials and CA the controller mounted beside the spec
// file, if it mounted any. Both are process-wide, which they can be here and could not in the
// controller: a deploy pod exists to perform one pull for one package
// (docs/agent/choice-registry-credentials.md).
//
// Nothing mounted is the ordinary case and not an error - a reachable registry needs neither.
func Reachability(specPath, cachePath string) error {
	dir := filepath.Dir(specPath)

	// packager.LoadOptions carries no credential function, so the only way in is oras' Docker
	// credential store, which reads $DOCKER_CONFIG before ~/.docker. The mounted Secret's
	// config.json key makes the mount directory a Docker config directory.
	switch _, err := os.Stat(filepath.Join(dir, deployspec.DockerConfigFileName)); {
	case err == nil:
		if err := os.Setenv("DOCKER_CONFIG", dir); err != nil {
			return fmt.Errorf("pointing DOCKER_CONFIG at the mounted credentials: %w", err)
		}
	case !errors.Is(err, fs.ErrNotExist):
		return err
	}

	caBundle, err := os.ReadFile(filepath.Join(dir, deployspec.CAFileName))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return err
	}
	return trustCA(caBundle, cachePath)
}

// trustCA makes caBundle trusted by every TLS client in this process, through SSL_CERT_FILE.
//
// Not by setting a root pool on http.DefaultTransport, which was the first attempt and does not
// work: the scheme probe inside `zoci` builds its own transport, and `packager.LoadOptions`
// offers nowhere to hand one in, so a pull fails verification while the default transport sits
// there correctly configured. SSL_CERT_FILE reaches whatever transport any dependency builds,
// because it is read by crypto/x509 itself.
//
// SSL_CERT_FILE *replaces* the system bundle rather than adding to it, so the file written here
// is the mounted CA concatenated with the image's own bundle - a mirror in front of a public
// registry keeps its public roots, which is the behaviour the controller side gets for free.
func trustCA(caBundle []byte, cachePath string) error {
	if pool := x509.NewCertPool(); !pool.AppendCertsFromPEM(caBundle) {
		// Refuse here rather than at a handshake, where nothing points at the cause.
		return fmt.Errorf("the mounted %s holds no PEM certificate", deployspec.CAFileName)
	}

	combined := append([]byte{}, caBundle...)
	if len(combined) > 0 && combined[len(combined)-1] != '\n' {
		combined = append(combined, '\n')
	}
	for _, path := range systemCertFiles {
		system, err := os.ReadFile(path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		combined = append(combined, system...)
		// crypto/x509 stops at the first one it finds, and so do we.
		break
	}

	// The cache volume, because the pod's root filesystem is read-only - the same reason
	// Run points zarf's temp directory there.
	dir := cachePath
	if dir == "" {
		dir = os.TempDir()
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	bundlePath := filepath.Join(dir, "caapz-ca-bundle.crt")
	if err := os.WriteFile(bundlePath, combined, 0o600); err != nil {
		return fmt.Errorf("writing the combined CA bundle: %w", err)
	}
	// Set before any TLS client is built: crypto/x509 reads the variable once, on the first
	// system-pool load, and caches the result.
	return os.Setenv("SSL_CERT_FILE", bundlePath)
}
