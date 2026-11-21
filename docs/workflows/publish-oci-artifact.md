# Publish the Cluster API Operator artifact

[`.github/workflows/publish-oci-artifact.yaml`](https://github.com/colonel-byte/cluster-api-addon-provider-zarf/blob/main/.github/workflows/publish-oci-artifact.yaml) publishes the provider's component manifests as one OCI artifact at `ghcr.io/colonel-byte/turtle/addon-zarf` - the form the Cluster API Operator, and so Rancher Turtles, installs a provider from inside an air gap. See the Rancher Turtles guide for the consuming side.

It is a reusable workflow, called from [`release.yaml`](release.md) in a job that `needs` goreleaser, because the artifact's manifests name the controller image this release pushes. It also runs on demand from the Actions tab against a tag.

The artifact is built by `go run ./magefiles/core release:ociArtifact <tag>` as an OCI image layout under `out/oci` - a directory, not a push, because the registry an air gap uses is on the far side of it; this workflow's ghcr.io target is the one registry that is not, being what a mirror copies *from*. The push is `oras cp --from-oci-layout`, and the pushed manifest is Cosign-signed **by digest**, never by tag.

## What it needs

The `COSIGN_KEY` / `COSIGN_PASS` secrets via `secrets: inherit`, and `packages: write` on its job to push the artifact and its signature.
