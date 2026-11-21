# The Zarf package is built from `out/`, and pins its version everywhere it appears

This repository ships a Zarf `v1beta1` package definition, [`zarf.yaml`](../../zarf.yaml), for management clusters with no route to `ghcr.io` - the same environments this provider deploys *into* are the ones it has to be *delivered* through. The file mirrors cluster-api-provider-cargoship's `zarf.yaml`, and its `docs/agent/choice-zarf-package.md` is the original of this argument. Two things about it look like oversights and are not.

## It deploys `out/addon-components.yaml`, not the kustomization

Zarf can run kustomize itself - a manifest takes `kustomize.files` as readily as `files` - so the package could point at `config/default` and skip `release:manifests` entirely. It does not, because then the provider would be assembled two ways: once by `release:manifests` for the operator who installs with `clusterctl`, and once by Zarf for the operator in an air gap. Both would be correct on the day they were written and free to drift afterwards. Pointing at the release artifact means the air-gapped install is the *same* artifact, and `release:zarfPackage` builds it first so there is nothing to forget.

The one kustomization Zarf does run, `manifest/turtle/addon`, assembles nothing a second way: it wraps that same `out/addon-components.yaml` (staged beside it by `release:zarfPackage`, gitignored there) into the ConfigMap the Cluster API Operator reads a provider out of.

## The version and image tags are literals, repeated

`###ZARF_PKG_TMPL_...###` does not reach an image reference in a `v1beta1` package: Zarf parses the reference during validation, which runs before `--set` is applied, and the create fails with an invalid-reference error - `zarf dev lint .` reports the same thing, so this is cheap to re-confirm.

So the version is written out as a literal in every file that needs it - `zarf.yaml` (once as the package version, twice as image tags), `manifest/turtle/capi-provider-addon.yaml`, `manifest/turtle/addon/kustomization.yaml` - and release-please rewrites all of them on a release: each file is in `extra-files` in [`release-please-config.json`](../../release-please-config.json), and each pinned line carries the `x-release-please-version` comment it looks for. Removing one of those comments is silent - the package still builds, pinning a tag a release ago.

`release:zarfPackage` reads the version back out of `zarf.yaml` and renders the component manifest at it, rather than taking a tag like `release:manifests` does. A tag parameter there would produce a package whose manifest names one version and whose image carries another, which fails at deploy time against the in-cluster registry rather than at build time.

## What it deliberately leaves out

Core Cluster API, Rancher Turtles, and every Zarf package this provider itself deploys. Each is somebody else's artifact with its own cadence, and bundling any of them would make this package a mirror of the whole air gap rather than of this provider. The `rancher-turtle-artifacts` component is `optional: true` for the related reason recorded inline: its `CAPIProvider` fails on a cluster without the Turtles CRDs, and the default deploy has to work for the clusterctl-shaped cluster too.
