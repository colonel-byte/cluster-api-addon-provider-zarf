# Staged packages

How to declare an ordered, multi-pass bootstrap - the same package deployed more than once with a different component selection each time, interleaved with another package doing the same - as one `ZarfPackageProxy`, and what the controller does with it per cluster. If you deploy one package per proxy to already-initialized clusters, you do not need this page; [`bootstrap-order.md`](bootstrap-order.md) explains when you do, and this page is how you write it down once you do.

A complete, runnable manifest for the worked example lives at [`config/samples/staged-rook-ceph.yaml`](https://github.com/colonel-byte/cluster-api-addon-provider-zarf/blob/main/config/samples/staged-rook-ceph.yaml) and is reproduced in full below.

## The shape

A staged proxy replaces `spec.package` with two fields that come together:

- **`spec.packages`** - the sources, as a map keyed by a name you choose. Hoisted out of the stage list so a package appearing in several stages carries exactly one version: a version bump is one edit, never two edits that can half-land.
- **`spec.stages`** - the ordered sequence over them. Each stage names a `packageRef` into the map, a component selection, and a `strategy` of its own.

Zarf-state initialization stays outside the list, on `spec.init` - it is gated on observed cluster state (`hasZarfState`), not on list position, so a cluster someone else already initialized skips it and the sequence simply begins at stage 1. `spec.init.packageRef` lets the init package come from the same `spec.packages` map, so declaring it for init and for a later stage is still one version in one place.

Three rules admission enforces that are worth knowing before you hit them:

| Rule                                            | Why                                                                                                               |
| ----------------------------------------------- | ----------------------------------------------------------------------------------------------------------------- |
| `deletionPolicy: Retain` is required            | Removal of a bootstrap sequence must not be something anybody gets by deleting an object                          |
| A stage's `strategy` is immutable once declared | Flipping `ApplyOnce` to `Reconcile` would re-run a history stage, which is the destructive direction              |
| `init.registry.mode` is immutable once set      | Switching a live cluster's registry mode re-addresses the registry under every release that templated the address |

## The worked example

The reference case from [`bootstrap-order.md`](bootstrap-order.md): a cluster with no storage and no registry, where storage is a Zarf package and the permanent registry needs somewhere to live. Four passes - seed storage from the ephemeral registry, stand up the permanent registry, then converge both on their end states - declared once, sequenced by the controller on every matching cluster.

The packages are the published ones under `ghcr.io/colonel-byte`: `zarf/init-slim`, the v1beta1 init package, and `zarf/csi-rook-ceph`, the storage package. The versions below were current when this page was written - check the registry for the tag to pin. In a real air gap both are staged into your internal registry first and the repositories below point there; the public references keep this example runnable as written.

```yaml
apiVersion: addons.colonel-byte.dev/v1alpha1
kind: ZarfPackageProxy
metadata:
  name: rook-ceph-bootstrap
  namespace: default
spec:
  clusterSelector:
    matchLabels:
      storage: rook-ceph
  # Required on a staged proxy: deleting this object must never uninstall a bootstrap.
  deletionPolicy: Retain
  packages:
    zarf-init:
      repository: ghcr.io/colonel-byte/zarf/init-slim
      version: v1.0.1
      api: v1beta1
    csi:
      repository: ghcr.io/colonel-byte/zarf/csi-rook-ceph
      version: v1.21.2-upstream
      api: v1beta1
  init:
    # Creates zarf-state when the cluster has none; skipped when someone else already did.
    autoInit: true
    packageRef: zarf-init
    components:
      - name: zarf-injector
      - name: zarf-seed-registry
    registry:
      # proxy mode is what lets the internal registry serve TLS; nodeport (the default)
      # forces its mTLS strategy to none. Immutable once set.
      mode: proxy
    # Proxy mode requires an image the node already holds - on first init there is no
    # registry to pull one from. A per-fleet value; a wrong one fails as an image pull.
    injectorImage: registry.k8s.io/kube-proxy:v1.31.0
    values:
      registry:
        extraEnvVars:
          # The seed phase disables persistence (no StorageClass exists yet), which also
          # drops the chart's filesystem storage driver - without this env the seed
          # registry crashloops with "no storage configuration provided". Measured live;
          # see "Measured on a live cluster" below.
          - name: REGISTRY_STORAGE_FILESYSTEM_ROOTDIRECTORY
            value: /var/lib/registry
  stages:
    - name: storage-seed
      packageRef: csi
      strategy: ApplyOnce
      components:
        - name: operator-images
        - name: cluster-images
        - name: operator-seed-chart
        - name: cluster-seed-chart
        - name: registry-bucket
    - name: registry
      packageRef: zarf-init
      strategy: Reconcile
      components:
        - name: zarf-registry
        - name: zarf-agent
    - name: storage-final
      packageRef: csi
      strategy: Reconcile
      components:
        - name: operator-images
        - name: cluster-images
        # The plain charts, not the seed ones - by now the agent is running and rewrites
        # image references itself, so the templated addresses are no longer needed.
        - name: operator-chart
        - name: cluster-chart
        - name: registry-bucket
```

Why the final stage installs the plain charts rather than merely re-pushing images, and why the seed charts exist at all, is measured and explained in [`bootstrap-order.md`](bootstrap-order.md#why-the-init-package-cannot-be-split) - the short version is that seed charts hard-code the registry address and the agent re-evaluates it, so leaving seed values in place is leaving scaffolding up.

## Measured on a live cluster

The worked example was run end to end against a CAPD workload cluster with no storage, no registry, and no zarf-state. Three things bit that nothing in the manifest predicts, each with the fix the example above now carries or the step you must take beside it:

- **Proxy mode's host ports are forbidden under the `baseline` Pod Security standard.** The injector (hostPort 5001) and the per-node registry proxy (hostPort 5000) sat at `FailedCreate` until the `zarf` namespace carried `pod-security.kubernetes.io/enforce: privileged`. The init deploy creates that namespace, so either create it with the label before the proxy matches the cluster, or set the label cluster-side policy-wise; without it the seed registry waits forever for an injector that will never schedule. The same standard bites storage provisioners: local-path's helper pod mounts a `hostPath` volume to create each node directory, so its namespace needs the same label or every PVC sits at `ProvisioningFailed` while the provisioner itself runs happily - bake the label into the namespace your storage package creates.
- **The seed registry needs its storage driver named when the seed stage runs alone.** The seed phase disables persistence - correct, no StorageClass exists yet - but that also drops `REGISTRY_STORAGE_FILESYSTEM_ROOTDIRECTORY` from the chart's env, and distribution refuses to start with no storage driver: a crashloop logging `no storage configuration provided`. The `init.values` entry in the example maps it back through the package's `.registry.extraEnvVars`. The all-components-at-once init path does not hit this, because the permanent registry's values immediately supersede the seed's on the same Helm release.
- **Leftovers from a previous non-Zarf install are an ownership conflict, not a merge.** A storage provisioner installed by hand earlier leaves cluster-scoped RBAC behind even after its namespace is deleted, and Helm refuses to adopt resources it does not own: the stage fails with `invalid ownership metadata`. Delete the leftovers (or wait for a `takeOwnership` knob); the stage retries once its Job is deleted or its spec edited.

Also observed, and worth knowing is normal: zarf writes zarf-state *early* in the init deploy, so a seed that fails after that point leaves a cluster whose `kubectl get zarfpackagereleases -o wide` already shows the declared mode in the `Registry` column while the release holds on the failure - the state is real, the registry behind it is not yet.

## How the controller runs it

Per matching cluster, in declared order, one stage at a time. Each stage is one of three states on each cluster:

- **Settled** - its applied record matches the current rendered spec, or it is `ApplyOnce` with any record at all. Settled stages are skipped.
- **In flight** - its deploy Job is running. The release reports `Deploying` and nothing else starts on that cluster.
- **Owed** - the first owed stage whose predecessors are all settled takes the cluster's deploy slot.

The consequences an operator plans around:

| You change                                   | What re-runs                                                                              |
| -------------------------------------------- | ----------------------------------------------------------------------------------------- |
| `csi`'s version                              | `storage-final`; `storage-seed` is `ApplyOnce` and stays put                              |
| `zarf-init`'s version                        | `registry`; the init step is held put by the state probe                                  |
| One stage's `components`, `values`, or `set` | That stage alone, and re-runs happen in declared order when more than one changed         |
| Nothing                                      | Nothing - a converged proxy is quiet                                                      |
| A failed stage's spec                        | That stage retries; an untouched failed stage holds the sequence with the failure visible |

A stage completes when its deploy Job completes - the controller does not judge whether the workload it installed is healthy. A package that reports success before its workload can serve is a bug in the package; [`bootstrap-order.md`](bootstrap-order.md#if-you-package-a-storage-driver-make-it-prove-itself-ready) says what that obliges of a package author.

Clusters that arrive already bootstrapped - by hand, by the Ansible collection, or re-created after a `clusterctl move` - are adopted rather than re-seeded: an `ApplyOnce` stage is satisfied by the cluster's own zarf deployed-package record, so the destructive re-run of seed values over a converged cluster does not happen.

## Editing the list once clusters have run it

`Reconcile` stages are desired state: insert, remove, and reorder them freely. `ApplyOnce` stages are history: once applied on any cluster, renaming one, removing it, or inserting a new `ApplyOnce` stage before it holds that cluster's release with reason `StageListDiverged` and a message naming the first divergence - because a rename reads as a removal plus an insertion, and the insertion would run. Appending at the end is always the unambiguous edit.

## Watching it

```sh
kubectl get zarfpackageproxies                 # Ready, Deployed/Clusters counts, Version (the last stage's)
kubectl get zarfpackageproxies -o wide         # adds Stages, Superseded, resolution detail
kubectl get zarfpackagereleases -o wide        # per cluster: Phase, Applied/Stages counts, Ready reason
```

Per-stage detail lives on the release: `status.stages[]` records each applied stage's identity and time, and the per-stage deploy Jobs are named `<release>-<stage>-deploy`, labelled with `addons.colonel-byte.dev/stage`. An `ApplyOnce` stage's finished Job is kept as the record of its run.

Holds you may see and what they mean:

| Ready reason          | What it means                                                                                               |
| --------------------- | ----------------------------------------------------------------------------------------------------------- |
| `WaitingForZarfState` | The cluster has no zarf-state and the next stage is not init-capable; `spec.init` (or someone) must init it |
| `WaitingForStageSpec` | A stage's package has not resolved; the proxy's `PackageResolved` condition says why                        |
| `StageListDiverged`   | The declared list diverges from this cluster's applied `ApplyOnce` history; the message names where         |
| `DeployFailed`        | A stage's Job failed; the message carries the pod's error, and editing that stage retries it                |
