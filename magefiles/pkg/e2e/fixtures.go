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
	"fmt"
	"net/http"
	"os"
	"path/filepath"

	"github.com/sigstore/cosign/v3/pkg/cosign"
	"github.com/zarf-dev/zarf/src/pkg/packager"
	"github.com/zarf-dev/zarf/src/pkg/signing"
	"github.com/zarf-dev/zarf/src/types"
	"oras.land/oras-go/v2/registry"
)

// The fixture packages Phase 2 of the e2e plan deploys, staged into the local registry by Up -
// the same staged-ahead-of-time motion an air-gapped environment uses for real packages. Built
// through the vendored Zarf SDK rather than the zarf CLI, so the topology needs no extra host
// binary and the fixtures come from the same library the controller resolves against.
const (
	// fixtureRepository is the repository prefix every fixture is pushed under. Zarf's Create
	// appends the package's own name and version, so a package lands at
	// <registry>/packages/<name>:<version>.
	fixtureRepository = "packages"

	// appPackage is the happy-path fixture: one ConfigMap, no images, so a deploy is fast and
	// its success is a single object to assert on the workload cluster.
	appPackage = "e2e-app"

	// driftPackage is the §3 tag-drift fixture, pushed twice at two versions into one
	// repository: the test moves driftVersion's tag onto the driftAltVersion manifest, which
	// is a content re-push under a pinned tag. One repository on purpose - a manifest may only
	// reference blobs its own repository holds, so re-tagging across repositories would be
	// rejected rather than drifting.
	driftPackage = "e2e-drift"

	// fixtureVersion is the version every fixture declares, and driftAltVersion the second one
	// the drift pair needs.
	fixtureVersion  = "0.1.0"
	driftAltVersion = "0.1.1"

	// initPackageRepository is the init package the auto-init path deploys, mirrored into the
	// local registry. init-slim rather than upstream's own `packages/init`, and the reason is
	// load-bearing: every init package upstream publishes is `zarf.dev/v1alpha1`, and the
	// v1alpha1 definition carries no component `service` names. Zarf's init orchestration -
	// start the injector, bootstrap the seed registry, stand up the registry and the agent -
	// switches entirely on those names, and the v1alpha1-to-internal conversion does not
	// synthesize them. A v1alpha1 init package deployed through the SDK therefore writes zarf
	// state, records the package, installs no chart, starts nothing, and reports success.
	// init-slim is `zarf.dev/v1beta1` and composes upstream's published v1beta1 component
	// configs, so the service names survive into the built package.
	initPackageRepository = "ghcr.io/colonel-byte/zarf/init-slim"

	// initPackageVersion is the init package version the auto-init fixtures deploy. It tracks
	// the zarf SDK pin in go.mod - init-slim versions follow the Zarf release its components
	// come from, so a bump to one without the other is the interesting failure.
	//
	// renovate: datasource=docker depName=ghcr.io/colonel-byte/zarf/init-slim extractVersion=^(?<version>v\d+\.\d+\.\d+)$
	initPackageVersion = "v1.0.1"

	// initPackageName is the package's own declared name, which is also the repository segment
	// it is mirrored under - PublishFromOCI requires the last path segment to match on both
	// sides, and zarf keys its deployed-package record on the same name.
	initPackageName = "init-slim"

	// infraNodeLabel is the label init-slim's registry affinity requires; its shipped values
	// pin the registry to nodes carrying it. Owned by that package, not by this repository -
	// if it changes there, this constant follows.
	infraNodeLabel = "adrp.xyz/is-infra"

	// cosignPassword is the fixture signing key's passphrase. The keypair is generated into the
	// gitignored workdir on every fresh topology, so this is a formality the cosign APIs
	// require rather than a secret: no private key is committed, and there is none to rotate.
	cosignPassword = "e2e"
)

// keyDir holds the fixture keypair that signs the packages, plus a second, unrelated pair the
// bad-signature negative test verifies against.
func keyDir() string { return filepath.Join(workDir, "keys") }

// ensureKeypairs generates both fixture keypairs into the gitignored workdir. Idempotent: an
// existing pair is kept, so re-running Up does not orphan packages already signed with it.
func ensureKeypairs() error {
	if err := os.MkdirAll(keyDir(), 0o700); err != nil {
		return err
	}
	for _, name := range []string{"cosign", "wrong"} {
		keyPath := filepath.Join(keyDir(), name+".key")
		if _, err := os.Stat(keyPath); err == nil {
			continue
		}
		keys, err := cosign.GenerateKeyPair(func(bool) ([]byte, error) {
			return []byte(cosignPassword), nil
		})
		if err != nil {
			return fmt.Errorf("generating the %s keypair: %w", name, err)
		}
		if err := os.WriteFile(keyPath, keys.PrivateBytes, 0o600); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(keyDir(), name+".pub"), keys.PublicBytes, 0o644); err != nil { //nolint:gosec // public key
			return err
		}
	}
	return nil
}

// fixtureDefinition renders a manifest-only package definition holding one ConfigMap whose data
// carries marker, so two builds of one package name differ in content - what the drift pair
// needs - while staying a single object to assert on after a deploy.
func fixtureDefinition(name, version, marker string) (zarfYAML, configMap string) {
	zarfYAML = fmt.Sprintf(`apiVersion: zarf.dev/v1beta1
kind: ZarfPackageConfig
metadata:
  name: %s
  version: %s
  description: CAAPZ e2e fixture - one ConfigMap, no images
components:
  - name: configmap
    manifests:
      - name: %s
        namespace: default
        files:
          - configmap.yaml
`, name, version, name)

	configMap = fmt.Sprintf(`apiVersion: v1
kind: ConfigMap
metadata:
  name: %s
  namespace: default
data:
  marker: %q
`, name, marker)
	return zarfYAML, configMap
}

// seedFixturePackages builds every fixture package and pushes it to the local registry, signed
// with the fixture key. Unconditionally rebuilt rather than probed for: create is cheap for a
// manifest-only package, and a staleness probe gets the answer wrong exactly when a fixture
// definition has changed.
func seedFixturePackages() error {
	fixtures := []struct {
		name    string
		version string
		marker  string
	}{
		{
			name:    appPackage,
			version: fixtureVersion,
			marker:  appPackage,
		},
		{
			name:    driftPackage,
			version: fixtureVersion,
			marker:  "original",
		},
		{
			name:    driftPackage,
			version: driftAltVersion,
			marker:  "re-pushed",
		},
	}
	for _, fixture := range fixtures {
		if err := createFixturePackage(fixture.name, fixture.version, fixture.marker); err != nil {
			return fmt.Errorf("creating fixture %s:%s: %w", fixture.name, fixture.version, err)
		}
	}
	return nil
}

// createFixturePackage writes the definition into the workdir and pushes the built, signed
// package to the local registry.
func createFixturePackage(name, version, marker string) error {
	dir := filepath.Join(workDir, "fixtures", name+"-"+version)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	zarfYAML, configMap := fixtureDefinition(name, version, marker)
	if err := os.WriteFile(filepath.Join(dir, "zarf.yaml"), []byte(zarfYAML), 0o644); err != nil { //nolint:gosec // fixture
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "configmap.yaml"), []byte(configMap), 0o644); err != nil { //nolint:gosec // fixture
		return err
	}

	key, err := filepath.Abs(filepath.Join(keyDir(), "cosign.key"))
	if err != nil {
		return err
	}
	signOpts := signing.DefaultSignBlobOptions()
	signOpts.Key = key
	signOpts.SkipConfirmation = true

	// cosign takes the passphrase from its own environment variable; the SDK exposes no
	// argument for it.
	if err := os.Setenv("COSIGN_PASSWORD", cosignPassword); err != nil {
		return err
	}

	opts := packager.CreateOptions{
		SignBlobOptions: &signOpts,
		SkipSBOM:        true,
		CachePath:       filepath.Join(workDir, "zarf-cache"),
		// PlainHTTP opts into per-host transport negotiation rather than forcing plaintext:
		// the probe finds HTTPS where it is served and plain HTTP on this test registry.
		RemoteOptions:    types.RemoteOptions{PlainHTTP: true},
		SkipVersionCheck: true,
	}
	// The output is the repository base; Create appends the package's name and version.
	_, err = packager.Create(context.Background(), dir,
		"oci://"+registryHostPort+"/"+fixtureRepository, opts)
	return err
}

// mirrorInitPackage copies upstream's init package into the local registry, registry to
// registry, so the auto-init path resolves and pulls it by the same stable name everything else
// in the topology uses. Skipped once the destination carries it: this is the one
// multi-hundred-megabyte transfer in the topology, and Up is meant to be re-runnable.
func mirrorInitPackage() error {
	repository := fixtureRepository + "/" + initPackageName
	mirrored, err := registryHasTag(repository, initPackageVersion)
	if err != nil {
		return err
	}
	if mirrored {
		fmt.Println("the init package is already mirrored; skipping")
		return nil
	}

	src, err := registry.ParseReference(initPackageRepository + ":" + initPackageVersion)
	if err != nil {
		return err
	}
	// PublishFromOCI requires the package name - the last path segment - to match on both
	// sides, which initPackageName is chosen to satisfy.
	dst, err := registry.ParseReference(registryHostPort + "/" + repository + ":" + initPackageVersion)
	if err != nil {
		return err
	}
	fmt.Println("mirroring", src.String(), "->", dst.String(), "(this is the big one)")
	return packager.PublishFromOCI(context.Background(), src, dst, packager.PublishFromOCIOptions{
		// The fixtures and the workload cluster are both amd64; the resolver defaults to it too.
		Architecture: "amd64",
		// Negotiated per host, so the ghcr.io read stays HTTPS while the local push is plain.
		RemoteOptions: types.RemoteOptions{PlainHTTP: true},
	})
}

// registryHasTag reports whether the local registry already serves repository:tag, over the
// registry HTTP API rather than an OCI client - one request, no auth, no manifest parsing.
func registryHasTag(repository, tag string) (bool, error) {
	url := fmt.Sprintf("http://%s/v2/%s/manifests/%s", registryHostPort, repository, tag)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodHead, url, nil)
	if err != nil {
		return false, err
	}
	// Without an Accept naming the index type, a registry answers 404 for a multi-arch tag it
	// does hold.
	req.Header.Set("Accept", "application/vnd.oci.image.index.v1+json, application/vnd.oci.image.manifest.v1+json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close() //nolint:errcheck // HEAD, no body to read
	return resp.StatusCode == http.StatusOK, nil
}
