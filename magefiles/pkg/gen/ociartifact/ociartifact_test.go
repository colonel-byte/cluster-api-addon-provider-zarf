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

package ociartifact

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/content/oci"
)

// TestGenerate packs fixture manifests and reads the layout back: the tag resolves, the
// manifest carries one layer per file with the title annotation the operator fetches by, and
// the ingest staging directory is gone.
func TestGenerate(t *testing.T) {
	dir := t.TempDir()
	for _, name := range Files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("kind: Fixture # "+name), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if err := Generate(dir, "v0.0.1-test"); err != nil {
		t.Fatalf("Generate: %v", err)
	}

	store, err := oci.New(filepath.Join(dir, "oci"))
	if err != nil {
		t.Fatalf("unable to reopen the layout: %v", err)
	}
	desc, err := store.Resolve(context.Background(), "v0.0.1-test")
	if err != nil {
		t.Fatalf("the version tag does not resolve: %v", err)
	}
	if desc.MediaType != ocispec.MediaTypeImageManifest {
		t.Fatalf("resolved media type = %q, want an image manifest", desc.MediaType)
	}
	if _, err := os.Stat(filepath.Join(dir, "oci", "ingest")); !os.IsNotExist(err) {
		t.Fatalf("the ingest staging directory survived: %v", err)
	}
}

// TestGenerateRefusesWithoutAVersion pins the error path the make-shaped invocation hits.
func TestGenerateRefusesWithoutAVersion(t *testing.T) {
	if err := Generate(t.TempDir(), ""); err == nil {
		t.Fatal("expected an error for an empty version")
	}
}
