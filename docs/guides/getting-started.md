# Getting started

What you end up with: this provider running in a Cluster API management cluster, and one Zarf package deployed to a workload cluster because you declared that it should be.

<!-- x-release-please-start-version -->
Every command here was run against `v0.1.2`.
<!-- x-release-please-end-version -->

Nothing in this guide reaches the public internet from the workload cluster. The packages come from a registry you stage them in, which is the point of the thing.

## What you need first

- A **Cluster API management cluster** with core Cluster API installed and at least one `Cluster` whose control plane is up. `clusterctl init` is what puts core Cluster API there; this provider installs beside it.
- **An OCI registry** the management cluster and the workload clusters can both reach, holding the packages you want to deploy. In an air gap this is the registry you staged everything into.
- **A Zarf-initialized workload cluster**, or a plan for getting one - read [Bootstrap order](bootstrap-order.md) before you deploy anything real, because the order is the part that bites.

## Install the provider

`clusterctl` learns about a provider it does not ship with from your own configuration file. Add this provider to `~/.config/cluster-api/clusterctl.yaml`:

<!-- x-release-please-start-version -->
```yaml
providers:
  - name: zarf
    url: https://github.com/colonel-byte/cluster-api-addon-provider-zarf/releases/v0.1.2/addon-components.yaml
    type: AddonProvider
```
<!-- x-release-please-end-version -->

Then install it:

<!-- x-release-please-start-version -->
```sh
clusterctl init --addon zarf:v0.1.2
```
<!-- x-release-please-end-version -->

`clusterctl` fetches `addon-components.yaml` and `metadata.yaml` from that release, so pinning the version in the URL and on the command line is what keeps an install reproducible. To check what it would apply without applying it:

<!-- x-release-please-start-version -->
```sh
clusterctl generate provider --addon zarf:v0.1.2
```
<!-- x-release-please-end-version -->

Confirm the controller is up:

```sh
kubectl -n caapz-system wait --for=condition=Available deploy/caapz-controller-manager --timeout=180s
```

### Air-gapped installs

The release also publishes the same component manifest as an OCI artifact and as a Zarf package, so the install itself can cross an air gap the same way everything else does. [Rancher Turtles](rancher-turtles.md) covers the OCI artifact path.

## Declare a package

A `ZarfPackageProxy` says *what* should be deployed and *where*. The controller matches it against `Cluster` objects by label, and stamps one `ZarfPackageRelease` per matching cluster to track that cluster's own outcome.

```yaml
apiVersion: addons.colonel-byte.dev/v1alpha1
kind: ZarfPackageProxy
metadata:
  name: podinfo
  namespace: default
spec:
  clusterSelector:
    matchLabels:
      zarf: enabled
  package:
    repository: registry.internal/packages/podinfo
    version: "1.0.0"
    api: v1beta1
```

Label a cluster to opt it in:

```sh
kubectl label cluster my-cluster zarf=enabled
```

Only `Cluster` objects in the proxy's own namespace match, unless the controller was started with `--cluster-namespaces` naming others. That default is deliberate: a label on a cluster in another team's namespace cannot pull your package onto it.

## Watch what happens

The proxy reports what it resolved; each release reports what happened on its own cluster.

```sh
kubectl get zarfpackageproxy,zarfpackagerelease
```

```sh
kubectl get zarfpackageproxy podinfo -o jsonpath='{range .status.conditions[*]}{.type}={.status} {.reason}{"\n"}{end}'
```

Three things on the proxy are worth reading before anything else:

| Field                   | What it tells you                                                                                                       |
| ----------------------- | ----------------------------------------------------------------------------------------------------------------------- |
| `status.resolvedDigest` | The digest the tag resolved to, pinned on first resolution. Every pull and deploy uses this, never the tag              |
| `status.packageKind`    | `ZarfPackageConfig`, or `ZarfInitConfig` for an init package                                                            |
| `PackageResolved`       | `False` here means nothing was deployed; the message says whether the registry, the signature, or the API claim refused |

And on a release:

```sh
kubectl get zarfpackagerelease podinfo-my-cluster -o jsonpath='{.status.phase}{"\n"}'
```

| Phase       | Meaning                                                                    |
| ----------- | -------------------------------------------------------------------------- |
| `Pending`   | Waiting on something - read the `Ready` condition's reason, which names it |
| `Deploying` | A deploy Job is running                                                    |
| `Deployed`  | The declared digest is on the cluster                                      |
| `Completed` | An `ApplyOnce` package ran its one deploy                                  |
| `Failed`    | The Job failed; the `Ready` condition carries the error the pod reported   |

A `Pending` release is the normal case worth understanding rather than worrying about. The usual reasons:

| Reason                   | What to do                                                                     |
| ------------------------ | ------------------------------------------------------------------------------ |
| `WaitingForZarfState`    | The cluster is not Zarf-initialized. See [Bootstrap order](bootstrap-order.md) |
| `WaitingForActiveDeploy` | Another deploy or removal Job holds that cluster's slot; one runs at a time    |
| `WaitingForDependency`   | A `dependsOn` package has not deployed to this cluster yet                     |
| `DependencyNotFound`     | A `dependsOn` entry names no proxy in this namespace - usually a typo          |

Each deploy runs in its own Job rather than in the controller, so when something fails the pod's logs are the record:

```sh
kubectl logs job/podinfo-my-cluster-deploy
```

## Verify the signature

A package's signature is only verified if you name a key to verify it against. Put the Cosign public key in a Secret and reference it:

```sh
kubectl create secret generic cosign-pubkey --from-file=cosign.pub=cosign.pub
```

```yaml
spec:
  package:
    repository: registry.internal/packages/podinfo
    version: "1.0.0"
    signatureKeySecretRef:
      name: cosign-pubkey
```

Naming a key makes verification a **precondition**: an unsigned package, a bad signature, or one that cannot be checked is refused in the controller before any Job exists, so unverified content never reaches a pod holding a cluster's kubeconfig. Verification binds to the pinned digest, so a re-pushed tag never inherits an earlier verification.

Leaving it unset verifies nothing, and the controller says so in its log rather than pretending otherwise. The `PackageSignatureVerified` condition is **absent** in that case, not `True`.

## Choose how it behaves over time

| `spec.strategy`       | Behaviour                                                                                         |
| --------------------- | ------------------------------------------------------------------------------------------------- |
| `Reconcile` (default) | Tracks the declared version. Bump `spec.package.version` and the package redeploys                |
| `ApplyOnce`           | Deploys exactly once per cluster and never re-executes, even if the spec changes later. For seeds |

| `spec.deletionPolicy` | On deleting the proxy or release                                                          |
| --------------------- | ----------------------------------------------------------------------------------------- |
| `Delete` (default)    | Runs a removal Job that uninstalls the package, then releases the finalizer               |
| `Retain`              | The release goes away immediately and the deployed package keeps running, owned by nobody |

`Retain` is the honest choice for large or foundational packages, where an orphaned-but-working package beats a half-removed one. An `ApplyOnce` package always behaves as `Retain` - a seed cannot be un-run.

Deleting a workload `Cluster` never waits on any of this. A cluster being torn down is infrastructure going away, so the controller drops its removal work rather than blocking the teardown on a cluster that can no longer answer.

## Updating a package

Edit `spec.package.version`. The controller re-resolves, re-verifies, and redeploys, and `status.resolvedDigest` moves to the new digest.

It will not move on its own. If someone re-pushes the same tag with different content, the proxy raises `TagDrift` with reason `TagContentChanged`, keeps running the digest it pinned, and waits for you - because a tag that changed under you is news, not an instruction.

## Where to go next

- [Bootstrap order](bootstrap-order.md) - the dependency order a first install has to follow, and why Zarf's own registry is the hard part
- [Rancher Turtles](rancher-turtles.md) - installing this provider through Turtles and the CAPI Operator
- [`docs/dev/architecture.md`](../dev/architecture.md) - the full design, if you want to know why any of this is shaped the way it is
