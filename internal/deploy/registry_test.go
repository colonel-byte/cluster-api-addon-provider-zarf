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
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/colonel-byte/cluster-api-addon-provider-zarf/internal/deployspec"
)

func TestReachabilityNothingMounted(t *testing.T) {
	// The ordinary case: a reachable registry mounts neither file, and that is not a failure.
	dir := t.TempDir()
	t.Setenv("DOCKER_CONFIG", "")
	if err := Reachability(filepath.Join(dir, deployspec.FileName), dir); err != nil {
		t.Fatalf("want no error with nothing mounted, got %v", err)
	}
	if got := os.Getenv("DOCKER_CONFIG"); got != "" {
		t.Fatalf("want DOCKER_CONFIG left alone, got %q", got)
	}
}

func TestReachabilityCredentials(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, deployspec.DockerConfigFileName, []byte(`{"auths":{}}`))
	t.Setenv("DOCKER_CONFIG", "")

	if err := Reachability(filepath.Join(dir, deployspec.FileName), dir); err != nil {
		t.Fatalf("Reachability: %v", err)
	}
	// The directory, not the file: oras reads $DOCKER_CONFIG/config.json, which is why the
	// Secret key has to be literally config.json.
	if got := os.Getenv("DOCKER_CONFIG"); got != dir {
		t.Fatalf("want DOCKER_CONFIG %q, got %q", dir, got)
	}
}

func TestReachabilityCABundle(t *testing.T) {
	// TMPDIR, so the bundle lands in the test's own directory rather than the real /tmp.
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	t.Setenv("SSL_CERT_FILE", "")

	ca := selfSignedPEM(t)
	write(t, dir, deployspec.CAFileName, ca)
	if err := Reachability(filepath.Join(dir, deployspec.FileName), dir); err != nil {
		t.Fatalf("Reachability: %v", err)
	}

	// SSL_CERT_FILE rather than a root pool on a transport of our own: the scheme probe inside
	// the SDK builds its own transport and takes none from us.
	bundlePath := os.Getenv("SSL_CERT_FILE")
	if bundlePath == "" {
		t.Fatal("want SSL_CERT_FILE pointed at the combined bundle")
	}
	combined, err := os.ReadFile(bundlePath)
	if err != nil {
		t.Fatalf("reading the combined bundle: %v", err)
	}
	if !bytes.Contains(combined, ca) {
		t.Fatal("want the mounted CA in the combined bundle")
	}
	// The system bundle is concatenated, not replaced - SSL_CERT_FILE substitutes for it, so a
	// mirror in front of a public registry would otherwise lose its public roots.
	if system, found := firstSystemBundle(); found {
		if !bytes.Contains(combined, system) {
			t.Fatal("want the image's own CA bundle in the combined bundle")
		}
		// And the pool actually parses as more than the one certificate.
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(combined) {
			t.Fatal("the combined bundle does not parse")
		}
	}
}

// firstSystemBundle is whichever bundle the host happens to ship, or not found on a host with
// none - a scratch container, which is a case worth not failing on.
func firstSystemBundle() ([]byte, bool) {
	for _, path := range systemCertFiles {
		if content, err := os.ReadFile(path); err == nil {
			return content, true
		}
	}
	return nil, false
}

func TestReachabilityCANotPEM(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, deployspec.CAFileName, []byte("not a certificate"))
	if err := Reachability(filepath.Join(dir, deployspec.FileName), dir); err == nil {
		t.Fatal("want an error for a mounted CA with no PEM in it")
	}
}

func write(t *testing.T, dir, name string, content []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), content, 0o600); err != nil {
		t.Fatalf("writing %s: %v", name, err)
	}
}

// selfSignedPEM is the smallest thing x509.AppendCertsFromPEM accepts.
func selfSignedPEM(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating a key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("signing the certificate: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}
