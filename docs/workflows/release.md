# goreleaser

[`.github/workflows/release.yaml`](https://github.com/colonel-byte/cluster-api-addon-provider-zarf/blob/main/.github/workflows/release.yaml) builds and publishes the release artifacts. It fires on any pushed tag, and in practice that tag comes from [`release-please.yaml`](release-please.md) merging its release pull request.

What it produces, from [`.goreleaser.yaml`](https://github.com/colonel-byte/cluster-api-addon-provider-zarf/blob/main/.goreleaser.yaml):

- one `linux` binary per `goarch`, version and commit baked in via `-ldflags -X`
- a `tar.gz` per architecture, named to match `uname` output
- the controller image `ghcr.io/colonel-byte/cluster-api-addon-provider-zarf-controller`, built for `linux/amd64` and `linux/arm64` from `containers/manager/Dockerfile` and cosign-signed (`docker_signs:`) - which is why the workflow sets up QEMU, Buildx, and a ghcr.io login before goreleaser runs, and why the job holds `packages: write`
- `out/addon-components.yaml` and `out/metadata.yaml` - the component manifest and provider metadata clusterctl fetches by name from the release assets, rendered by the `release:manifests` before hook at the release version and checked for presence after publish
- `checksums.txt`, an SBOM per archive, a cosign bundle per archive and for the checksums file
- a build provenance attestation over `checksums.txt`, uploaded to the release

After goreleaser succeeds on a `v*` tag, two more jobs call the air-gapped publish workflows as reusable workflows with `secrets: inherit`: [`publish-oci-artifact.yaml`](publish-oci-artifact.md) (the Cluster API Operator artifact) and then [`publish-zarf-package.yaml`](publish-zarf-package.md) (the Zarf package, which embeds that artifact and so waits for it). `needs:` is what sequences them - the release *event* fires before the images they pull exist.

## What it needs

| Secret        | What it is                                                            |
| ------------- | --------------------------------------------------------------------- |
| `COSIGN_KEY`  | The private half of the keypair published as `cosign.pub` at the root |
| `COSIGN_PASS` | That key's password                                                   |

Those are the **secret** names. The workflow maps them onto the environment variables `.goreleaser.yaml` reads, `COSIGN_PRIVATE_KEY` and `COSIGN_PASSWORD`, which are cosign's own and are not renameable. Changing one side without the other produces an unsigned-key error at the signing step, not a missing-secret error.

Both are required. The `signs:` blocks have no "unsigned if the key is missing" fallback, deliberately - a release that is quietly unsigned is worse than one that did not happen - so a missing secret fails the run rather than producing unsigned archives.

## Three things that fail quietly rather than loudly

- **`fetch-depth: 0`** is required, not a precaution. Goreleaser reads tag history for the version and the changelog, and a shallow clone gives it neither.
- **The `-ldflags -X` symbol paths have to exist.** A `-X` naming a variable the code does not declare is silently ignored, and the binary reports the zero value for its own version. Check with `go doc <module>/config` rather than from memory after moving or renaming those variables.
- **Syft is installed before goreleaser runs**, because the `sboms:` block shells out to it. Remove the `sboms:` block or the setup step together; the leftover half is an error in the middle of a release.

## Running it against a tag that does not exist

`goreleaser release --snapshot --clean` builds everything locally without publishing, signing, or needing a tag, and `--skip=sign` drops the cosign steps if the key is not to hand. That is the way to check a `.goreleaser.yaml` change before it is a release.
