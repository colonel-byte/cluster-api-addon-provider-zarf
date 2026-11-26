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

package resolver

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"testing"
	"time"
)

func TestClientOptionsCredentials(t *testing.T) {
	config := []byte(`{"auths":{"registry.internal:5000":{"auth":"emFyZi1wdWxsOmh1bnRlcjI="}}}`)
	opts, err := clientOptions(Options{DockerConfigJSON: config})
	if err != nil {
		t.Fatalf("clientOptions: %v", err)
	}
	if opts.CredentialFunc == nil {
		t.Fatal("want a credential function for a config with auths")
	}
	cred, err := opts.CredentialFunc(context.Background(), "registry.internal:5000")
	if err != nil {
		t.Fatalf("resolving the credential: %v", err)
	}
	if cred.Username != "zarf-pull" || cred.Password != "hunter2" {
		t.Fatalf("want zarf-pull/hunter2, got %s/%s", cred.Username, cred.Password)
	}

	// A host the config says nothing about gets no credential rather than another host's.
	other, err := opts.CredentialFunc(context.Background(), "ghcr.io")
	if err != nil {
		t.Fatalf("resolving an unnamed host: %v", err)
	}
	if other.Username != "" || other.Password != "" {
		t.Fatalf("want an empty credential for an unnamed host, got %s/%s", other.Username, other.Password)
	}
}

func TestClientOptionsNoMaterial(t *testing.T) {
	// Nothing named leaves both hooks nil, which is what makes zoci fall back to its own
	// defaults - the behaviour every proxy written before this API had.
	opts, err := clientOptions(Options{PlainHTTP: true})
	if err != nil {
		t.Fatalf("clientOptions: %v", err)
	}
	if opts.CredentialFunc != nil {
		t.Error("want no credential function")
	}
	if opts.Transport != nil {
		t.Error("want no transport")
	}
	if !opts.PlainHTTP {
		t.Error("want PlainHTTP carried through")
	}
}

func TestClientOptionsCABundle(t *testing.T) {
	ca := selfSignedPEM(t)
	opts, err := clientOptions(Options{CABundle: ca, InsecureSkipTLSVerify: true})
	if err != nil {
		t.Fatalf("clientOptions: %v", err)
	}
	if opts.Transport == nil {
		t.Fatal("want a transport for a named CA")
	}
	if opts.Transport.TLSClientConfig.RootCAs == nil {
		t.Fatal("want a root pool on the transport")
	}
	if !opts.InsecureSkipTLSVerify {
		t.Error("want InsecureSkipTLSVerify carried through")
	}
}

func TestClientOptionsCABundleNotPEM(t *testing.T) {
	// A ConfigMap key holding something that is not a certificate is a refusal here rather
	// than a TLS handshake failure with nothing pointing at the cause.
	if _, err := clientOptions(Options{CABundle: []byte("not a certificate")}); err == nil {
		t.Fatal("want an error for a bundle with no PEM in it")
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
