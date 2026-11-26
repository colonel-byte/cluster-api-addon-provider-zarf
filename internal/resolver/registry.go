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
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"

	"github.com/zarf-dev/zarf/src/pkg/zoci"
	"github.com/zarf-dev/zarf/src/types"
	"oras.land/oras-go/v2/registry/remote/credentials"
)

// clientOptions turns one resolution's credentials and CA into the per-call hooks
// zoci.NewRemote takes - never anything process-wide, because this controller resolves every
// proxy in the cluster and two of them may name two registries with two credentials
// (docs/agent/choice-registry-credentials.md).
func clientOptions(opts Options) (zoci.RemoteClientOptions, error) {
	clientOpts := zoci.RemoteClientOptions{
		RemoteOptions: types.RemoteOptions{
			PlainHTTP:             opts.PlainHTTP,
			InsecureSkipTLSVerify: opts.InsecureSkipTLSVerify,
		},
	}

	if len(opts.DockerConfigJSON) > 0 {
		// oras' own store, so the host matching - including docker.io's registry-to-server
		// address aliasing - is upstream's rather than reimplemented here.
		store, err := credentials.NewMemoryStoreFromDockerConfig(opts.DockerConfigJSON)
		if err != nil {
			return zoci.RemoteClientOptions{}, fmt.Errorf("reading the registry credentials: %w", err)
		}
		clientOpts.CredentialFunc = credentials.Credential(store)
	}

	if len(opts.CABundle) > 0 {
		transport, err := transportWithCA(opts.CABundle)
		if err != nil {
			return zoci.RemoteClientOptions{}, err
		}
		clientOpts.Transport = transport
	}

	return clientOpts, nil
}

// transportWithCA clones the default transport and adds caBundle to the system roots. Added to,
// not replacing: an operator naming their internal CA must not lose the public roots a mirror
// in front of a public registry still needs.
func transportWithCA(caBundle []byte) (*http.Transport, error) {
	pool, err := x509.SystemCertPool()
	if err != nil {
		// A scratch image with no bundle at all - the internal CA alone is still the answer.
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM(caBundle) {
		return nil, fmt.Errorf("the CA bundle holds no PEM certificate")
	}
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, fmt.Errorf("http.DefaultTransport is not an *http.Transport")
	}
	transport := base.Clone()
	transport.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	return transport, nil
}
