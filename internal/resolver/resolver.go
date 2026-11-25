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

// Package resolver turns a proxy's package reference into a pinned digest and a verified,
// distilled package definition - §6 step 4 of docs/dev/architecture.md, using exactly the SDK
// call chain the M1.5 spike proved (dist/spike-zarf-sdk.md): resolve the tag, pull only the
// metadata layers (~1 KB - zarf.yaml, checksums, signature bundle), and verify the Cosign
// signature from them. No package layer is ever fetched, so the controller needs no
// package-sized disk, and verification binds to the digest - a re-pushed tag never inherits a
// prior verification.
package resolver

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/zarf-dev/zarf/src/api"
	"github.com/zarf-dev/zarf/src/pkg/packager/layout"
	"github.com/zarf-dev/zarf/src/pkg/signing"
	"github.com/zarf-dev/zarf/src/pkg/zoci"
)

// Options configure one resolution.
type Options struct {
	// CosignPublicKey is the PEM public key from the proxy's signatureKeySecretRef secret
	// (its cosign.pub key). Empty verifies nothing - the §6 trust model's "empty verifies
	// nothing": the package loads unverified, exactly as an unsigned one would.
	CosignPublicKey []byte

	// PlainHTTP speaks HTTP to the registry, for test-only registries behind the
	// --registry-plain-http manager flag. Never the default.
	PlainHTTP bool

	// Architecture selects the package's platform in a multi-arch index. Empty means amd64.
	// shortcut: no per-proxy or detected architecture yet - the §4D detection source is a
	// named backlog item; widen this when it lands.
	Architecture string
}

// Result is a resolution's outcome: the pinned digest and the distilled facts later milestones
// gate on, never the SDK's own types.
type Result struct {
	// Digest is the package manifest's digest (sha256:...) - what status.resolvedDigest pins
	// and every later pull and deploy uses in place of the tag.
	Digest string

	// Verified is true when a key was given and the Cosign signature checked out against the
	// pinned digest. False with an empty key; a key that does not verify is an error, not a
	// false.
	Verified bool

	// Name is the package's declared name - the identity the ApplyOnce record is keyed on.
	Name string

	// IsInitPackage is true for kind ZarfInitConfig - what §6 step 5's init-first eligibility
	// gate reads.
	IsInitPackage bool

	// APIVersion is the package's declared schema version, "v1alpha1" or "v1beta1" - what the
	// §4C spec.package.api claim is checked against. The legacy empty form reads as v1alpha1.
	APIVersion string
}

// Interface is what the reconciler depends on, so envtest can substitute a stub - a real
// resolution needs a registry no API-server test has.
type Interface interface {
	Resolve(ctx context.Context, repository, version string, opts Options) (Result, error)
	ResolveDigest(ctx context.Context, repository, version string, opts Options) (string, error)
}

// Resolver implements Interface against a real registry.
type Resolver struct{}

var _ Interface = Resolver{}

func remote(ctx context.Context, repository, version string, opts Options) (*zoci.Remote, error) {
	arch := opts.Architecture
	if arch == "" {
		arch = "amd64"
	}
	// OS "multi" is how zarf publishes package platforms - verified in the spike.
	platform := ocispec.Platform{OS: "multi", Architecture: arch}
	return zoci.NewRemote(ctx, fmt.Sprintf("oci://%s:%s", repository, version), platform,
		zoci.RemoteClientOptions{PlainHTTP: opts.PlainHTTP})
}

// ResolveDigest resolves the tag to its current digest and nothing more - the cheap read the
// TagContentChanged drift check (§3) runs on every reconcile.
func (Resolver) ResolveDigest(ctx context.Context, repository, version string, opts Options) (string, error) {
	rmt, err := remote(ctx, repository, version, opts)
	if err != nil {
		return "", err
	}
	root, err := rmt.ResolveRoot(ctx)
	if err != nil {
		return "", err
	}
	return root.Digest.String(), nil
}

// Resolve pins the digest, pulls the metadata layers, verifies the signature when a key is
// given, and distills the package definition.
func (Resolver) Resolve(ctx context.Context, repository, version string, opts Options) (Result, error) {
	rmt, err := remote(ctx, repository, version, opts)
	if err != nil {
		return Result{}, err
	}
	root, err := rmt.ResolveRoot(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("resolving %s:%s: %w", repository, version, err)
	}

	manifest, err := rmt.FetchRoot(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("fetching the root manifest: %w", err)
	}
	// The zero-value selection is exactly the metadata set (PackageAlwaysPull): zarf.yaml,
	// checksums, and the signature bundle. Nothing component- or image-sized.
	layers, err := zoci.AssembleLayers(ctx, manifest, rmt.Repo(), zoci.LayerSelection{})
	if err != nil {
		return Result{}, fmt.Errorf("assembling the metadata layers: %w", err)
	}

	dir, err := os.MkdirTemp("", "caapz-resolve-*")
	if err != nil {
		return Result{}, err
	}
	defer os.RemoveAll(dir) //nolint:errcheck // best-effort temp cleanup

	// The package's own files get a subdirectory of their own, because zarf's loader validates
	// the directory against checksums.txt and refuses any file not listed in it - so anything
	// this function writes for its own purposes, the public key below, has to sit beside the
	// package rather than in it.
	pkgDir := filepath.Join(dir, "package")
	if err := os.Mkdir(pkgDir, 0o700); err != nil {
		return Result{}, err
	}

	if _, err := rmt.PullPackage(ctx, pkgDir, 2, layers...); err != nil {
		return Result{}, fmt.Errorf("pulling the metadata layers: %w", err)
	}

	layoutOpts := layout.PackageLayoutOptions{
		// VerifyNever with no key, by design: VerifyAlways with no material errors on signed
		// and unsigned packages alike, and "empty verifies nothing" means loading anyway.
		VerificationStrategy: layout.VerifyNever,
		IsPartial:            true,
	}
	if len(opts.CosignPublicKey) > 0 {
		keyPath := filepath.Join(dir, "cosign.pub")
		if err := os.WriteFile(keyPath, opts.CosignPublicKey, 0o600); err != nil {
			return Result{}, err
		}
		verify := signing.DefaultVerifyBlobOptions()
		verify.Key = keyPath
		layoutOpts.VerificationStrategy = layout.VerifyAlways
		layoutOpts.VerifyBlobOptions = &verify
	}

	pkgLayout, err := layout.LoadFromDir(ctx, pkgDir, layoutOpts)
	if err != nil {
		return Result{}, fmt.Errorf("loading and verifying the package metadata: %w", err)
	}
	definition := pkgLayout.Definition()

	apiVersion := definition.APIVersion
	if apiVersion == "" {
		// The legacy form predates the field; zarf reads it as v1alpha1.
		apiVersion = "zarf.dev/v1alpha1"
	}
	return Result{
		Digest:        root.Digest.String(),
		Verified:      len(opts.CosignPublicKey) > 0,
		Name:          definition.Metadata.Name,
		IsInitPackage: definition.Kind == api.ZarfInitConfig,
		APIVersion:    schemaVersion(apiVersion),
	}, nil
}

// schemaVersion reduces a package apiVersion ("zarf.dev/v1beta1") to the bare version the
// spec.package.api enum speaks ("v1beta1").
func schemaVersion(apiVersion string) string {
	for i := len(apiVersion) - 1; i >= 0; i-- {
		if apiVersion[i] == '/' {
			return apiVersion[i+1:]
		}
	}
	return apiVersion
}
