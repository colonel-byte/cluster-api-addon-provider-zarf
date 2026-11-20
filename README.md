# cluster-api-addon-provider-zarf
[![Latest Release](https://img.shields.io/github/v/release/colonel-byte/cluster-api-addon-provider-zarf)](https://github.com/colonel-byte/cluster-api-addon-provider-zarf/releases)
[![Build Status](https://img.shields.io/github/actions/workflow/status/colonel-byte/cluster-api-addon-provider-zarf/release.yaml)](https://github.com/colonel-byte/cluster-api-addon-provider-zarf/actions/workflows/release.yaml)
[![OpenSSF Scorecard](https://api.scorecard.dev/projects/github.com/colonel-byte/cluster-api-addon-provider-zarf/badge)](https://securityscorecards.dev/viewer/?uri=github.com/colonel-byte/cluster-api-addon-provider-zarf)

A Cluster API add-on provider for [Zarf](https://github.com/zarf-dev/zarf) packages (CAAPZ). It automates the deployment, day-2 lifecycle, and one-time initialization ("seed") of self-contained Zarf packages onto CAPI workload clusters in air-gapped enterprise environments - the Zarf analogue of [cluster-api-addon-provider-helm](https://github.com/kubernetes-sigs/cluster-api-addon-provider-helm), whose `HelmChartProxy` / `HelmReleaseProxy` split it follows.

> **Status: early development.** The CRDs and controller are being built; nothing is released yet.

## How it works

CAAPZ runs as a controller in the management cluster. A platform team declares a `ZarfPackageProxy` naming a package in an internal OCI registry and a `clusterSelector`; for every matching CAPI `Cluster`, the controller pulls the package, verifies its Cosign signature, resolves the cluster's kubeconfig secret, deploys the package over the wire via Zarf's Go SDK, and records the result in a per-cluster `ZarfPackageRelease` owned by that `Cluster`. The full design - dependency ordering, `ApplyOnce` seed packages, auto-init, non-blocking cluster deletion - is in [`docs/dev/architecture.md`](docs/dev/architecture.md).

## Custom resources

| Kind                 | Written by     | What it is                                                                      |
| -------------------- | -------------- | ------------------------------------------------------------------------------- |
| `ZarfPackageProxy`   | Platform team  | The desired package state across every cluster matching its selector            |
| `ZarfPackageRelease` | The controller | One cluster's deployment of one proxy - its phase, deployed version, conditions |

```yaml
apiVersion: addons.cluster.x-k8s.io/v1alpha1
kind: ZarfPackageProxy
metadata:
  name: security-baseline
spec:
  clusterSelector:
    matchLabels:
      environment: production
  package:
    repository: registry.internal.example.com/packages/security-baseline
    version: "1.4.2"
    signatureKeySecretRef:
      name: cosign-pubkey
  strategy: Reconcile
```

`strategy: Reconcile` keeps matching clusters converged on the declared version; `strategy: ApplyOnce` deploys exactly once per cluster and never re-executes, for bootstrap and seed payloads.

## Installation

Not yet published. Once released, the provider installs into a management cluster from the manifests under `config/`.

## Development

Build and generator targets run through mage; the build is vendored and works offline.

```sh
go build ./cmd/...                          # build the manager
go run ./magefiles/core -l                  # list every mage target
go run ./magefiles/core dev:vendor          # go mod tidy, then go mod vendor
go run ./magefiles/core generate:document   # regenerate the mdBook pages
```

See [`AGENTS.md`](https://github.com/colonel-byte/cluster-api-addon-provider-zarf/blob/main/AGENTS.md) for the repository rules and [`docs/dev/`](docs/dev/README.md) for developer notes.
