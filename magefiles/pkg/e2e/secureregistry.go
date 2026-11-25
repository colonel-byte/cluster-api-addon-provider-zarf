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
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/magefile/mage/sh"
	"golang.org/x/crypto/bcrypt"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/retry"
)

// The second registry in the topology: HTTPS with a certificate no public root signs, behind
// basic auth. It exists so the registry-credential and CA-bundle fields have something to be
// true about - on the plain registry every one of them is a no-op, and a test that cannot fail
// without them proves nothing.
//
// Deliberately a separate container rather than auth on the existing one: every other test in
// the suite resolves against a registry that needs nothing, and that unauthenticated path is
// itself worth keeping covered.
const (
	// secureRegistryHost is the hostname the certificate is issued for and the repository
	// string names - in-cluster and from the host alike, so one spelling works everywhere.
	secureRegistryHost = "caapz-e2e-secure-registry:5000"
	secureRegistryName = "caapz-e2e-secure-registry"

	// secureRegistryHostPort publishes it for the mage process' own push. The container's
	// own port stays 5000 so the published and in-cluster spellings agree on it.
	secureRegistryHostPort = "127.0.0.1:5002"

	// The credentials the registry is seeded with and the suite authenticates as. A throwaway
	// pair in a container that lives as long as a test topology.
	secureRegistryUser     = "caapz"
	secureRegistryPassword = "e2e-registry-password"
)

// secureRegistryDir holds the generated CA, the serving keypair, and the htpasswd file - bind
// mounted into the registry container, and read back by e2e:run to hand the suite the CA.
func secureRegistryDir() string { return filepath.Join(workDir, "secure-registry") }

// ensureSecureRegistry generates the CA and credentials, then starts the registry on the kind
// network. Idempotent in the same way ensureRegistry is: a running container is left alone, so
// the certificate the suite already trusts stays the one being served.
func ensureSecureRegistry() error {
	if running, err := sh.Output("docker", "inspect", "-f", "{{.State.Running}}", secureRegistryName); err == nil &&
		strings.TrimSpace(running) == "true" {
		return nil
	}
	bestEffort("docker", "rm", "-f", secureRegistryName)

	if err := generateSecureRegistryMaterial(); err != nil {
		return err
	}
	dir, err := filepath.Abs(secureRegistryDir())
	if err != nil {
		return err
	}
	if err := sh.RunV("docker", "run", "-d", "--restart=always",
		"-p", secureRegistryHostPort+":5000",
		"--name", secureRegistryName,
		"-v", dir+":/material:ro",
		"-e", "REGISTRY_HTTP_TLS_CERTIFICATE=/material/tls.crt",
		"-e", "REGISTRY_HTTP_TLS_KEY=/material/tls.key",
		"-e", "REGISTRY_AUTH=htpasswd",
		"-e", "REGISTRY_AUTH_HTPASSWD_REALM=caapz-e2e",
		"-e", "REGISTRY_AUTH_HTPASSWD_PATH=/material/htpasswd",
		registryImage); err != nil {
		return err
	}
	// The kind network, so the hostname the certificate names resolves from controller and
	// deploy pods as well as from here. "already exists" is the idempotent case.
	if err := sh.RunV("docker", "network", "connect", "kind", secureRegistryName); err != nil {
		connected, inspectErr := sh.Output("docker", "network", "inspect", "kind",
			"-f", "{{range .Containers}}{{.Name}} {{end}}")
		if inspectErr != nil || !strings.Contains(connected, secureRegistryName) {
			return err
		}
	}
	return waitSecureRegistry()
}

// generateSecureRegistryMaterial writes the CA, a certificate for secureRegistryHost signed by
// it, and the htpasswd file. Generated per topology rather than committed: a private key in a
// repository is a finding in every scanner that reads it, and these are worthless anyway.
func generateSecureRegistryMaterial() error {
	dir := secureRegistryDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	caTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "caapz e2e registry CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(365 * 24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		return err
	}
	ca, err := x509.ParseCertificate(caDER)
	if err != nil {
		return err
	}

	serverKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	serverTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: secureRegistryName},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		// Both spellings: the container name is what pods dial, and 127.0.0.1 is what the
		// mage process' own push reaches through the published port.
		DNSNames:    []string{secureRegistryName},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
	}
	serverDER, err := x509.CreateCertificate(rand.Reader, serverTemplate, ca, &serverKey.PublicKey, caKey)
	if err != nil {
		return err
	}
	serverKeyDER, err := x509.MarshalECPrivateKey(serverKey)
	if err != nil {
		return err
	}

	hashed, err := bcrypt.GenerateFromPassword([]byte(secureRegistryPassword), bcrypt.DefaultCost)
	if err != nil {
		return err
	}

	files := map[string][]byte{
		// The chain, server certificate first: the registry serves it as given, and a client
		// trusting only the CA still needs the leaf's issuer in the handshake.
		"tls.crt": append(
			pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: serverDER}),
			pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})...),
		"tls.key":  pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: serverKeyDER}),
		"ca.crt":   pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}),
		"htpasswd": []byte(secureRegistryUser + ":" + string(hashed) + "\n"),
	}
	for name, content := range files {
		mode := os.FileMode(0o644)
		if name == "tls.key" {
			mode = 0o600
		}
		// World-readable on purpose for the rest: the registry container runs as its own
		// unprivileged user and reads them through a read-only bind mount.
		if err := os.WriteFile(filepath.Join(dir, name), content, mode); err != nil {
			return err
		}
	}
	// The key has to be readable by the container's user too; the bind mount carries the host
	// mode through.
	return os.Chmod(filepath.Join(dir, "tls.key"), 0o644) //nolint:gosec // throwaway test key
}

// secureRegistryTransport dials the secure registry trusting only the generated CA - which is
// also what proves the CA is doing the work rather than a permissive default.
func secureRegistryTransport() (*http.Transport, error) {
	caPEM, err := os.ReadFile(filepath.Join(secureRegistryDir(), "ca.crt"))
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("the generated CA is not PEM")
	}
	transport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, fmt.Errorf("http.DefaultTransport is not an *http.Transport")
	}
	transport = transport.Clone()
	transport.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	return transport, nil
}

// waitSecureRegistry blocks until the registry answers /v2/ with the 401 an authenticated
// registry answers an anonymous request with - which is both "it is up" and "auth is on".
func waitSecureRegistry() error {
	transport, err := secureRegistryTransport()
	if err != nil {
		return err
	}
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
	url := "https://" + secureRegistryHostPort + "/v2/"
	deadline := time.Now().Add(2 * time.Minute)
	var last error
	for time.Now().Before(deadline) {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
		if err != nil {
			return err
		}
		resp, err := client.Do(req)
		if err == nil {
			resp.Body.Close() //nolint:errcheck // a readiness probe's body is never read
			if resp.StatusCode == http.StatusUnauthorized {
				return nil
			}
			last = fmt.Errorf("%s answered %s, want 401 - is auth configured?", url, resp.Status)
		} else {
			last = err
		}
		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("the secure registry never came up: %w", last)
}

// secureRepository is the oras handle on one repository in the secure registry, with the
// credentials and the CA applied.
func secureRepository(reference string) (*remote.Repository, error) {
	repo, err := remote.NewRepository(reference)
	if err != nil {
		return nil, err
	}
	transport, err := secureRegistryTransport()
	if err != nil {
		return nil, err
	}
	repo.Client = &auth.Client{
		Client: &http.Client{Transport: retry.NewTransport(transport)},
		Cache:  auth.NewCache(),
		Credential: auth.StaticCredential(repo.Reference.Registry, auth.Credential{
			Username: secureRegistryUser,
			Password: secureRegistryPassword,
		}),
	}
	return repo, nil
}

// seedSecureRegistry copies the app fixture from the plain registry into the secure one, as a
// plain OCI graph copy. The zarf SDK's own publish takes no credentials - the same gap the
// deploy pod works around with a mounted Docker config - and a package is an OCI artifact, so
// oras copies it without needing to understand it.
func seedSecureRegistry() error {
	ctx := context.Background()
	reference := fixtureRepository + "/" + appPackage + ":" + fixtureVersion

	dst, err := secureRepository(secureRegistryHostPort + "/" + reference)
	if err != nil {
		return err
	}
	if _, err := dst.Resolve(ctx, fixtureVersion); err == nil {
		fmt.Println("the app fixture is already in the secure registry; skipping")
		return nil
	}

	src, err := remote.NewRepository(registryHostPort + "/" + reference)
	if err != nil {
		return err
	}
	src.PlainHTTP = true

	fmt.Println("copying", reference, "into the secure registry")
	_, err = oras.Copy(ctx, src, fixtureVersion, dst, fixtureVersion, oras.DefaultCopyOptions)
	return err
}
