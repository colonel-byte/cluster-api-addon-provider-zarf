# Publish the Zarf package

[`.github/workflows/publish-zarf-package.yaml`](https://github.com/colonel-byte/cluster-api-addon-provider-zarf/blob/main/.github/workflows/publish-zarf-package.yaml) builds and publishes the air-gapped install artifact: the Zarf package [`zarf.yaml`](https://github.com/colonel-byte/cluster-api-addon-provider-zarf/blob/main/zarf.yaml) defines, pushed to `ghcr.io/colonel-byte/zarf/cluster-api-addon-provider-zarf`.

It is a reusable workflow, called from [`release.yaml`](release.md) in a job that `needs` goreleaser and the operator-artifact publish - never keyed off the release event, which fires before the images the package pulls exist. It also runs on demand from the Actions tab against a tag, which is how a release's package is republished without re-running the release.

One package per architecture (`amd64`, `arm64`), published serially because both land on one architecture-less Zarf reference. Each build goes through `go run ./magefiles/core release:zarfPackage`, so the component manifest inside the package is rendered at the version `zarf.yaml` pins - a guard step fails the run if that version and the tag disagree, which is what a lost `x-release-please-version` comment looks like. The package is signed with the repository's Cosign key and verified against [`cosign.pub`](https://github.com/colonel-byte/cluster-api-addon-provider-zarf/blob/main/cosign.pub) before the push.

## What it needs

The same `COSIGN_KEY` / `COSIGN_PASS` secrets as [`release.yaml`](release.md), passed through `secrets: inherit`, and `packages: write` on its job to push.
