# Bootstrap order

The order a first install has to follow, and why. If you deploy one package to one already-initialized cluster, you do not need this page. If you are standing up clusters from nothing - which is the case this provider exists for - the ordering is the part that fails, and it fails by waiting rather than by erroring.

This page is the declarative counterpart to [`zarf-bundle-order.yml`](https://github.com/colonel-byte/ansible-collection-zarf/blob/main/ansible/examples/playbooks/zarf-bundle-order.yml) in the Ansible collection, which performs the same ordering imperatively against a single cluster. The constraint is identical; what differs is that you describe the end state and the controller sequences it, across every matching cluster, instead of you sequencing it once per cluster.

## The constraint

Zarf's own internal registry wants persistent storage. Your storage provider is a package, and a package's images are pulled from a registry. On a cluster with neither, that is a cycle:

```
zarf init ──needs──> a StorageClass ──provided by──> a storage package ──needs──> a registry ──provided by──> zarf init
```

Nothing reports this as an error. `zarf init` stands up its injector and a temporary seed registry, then the permanent registry's `PersistentVolumeClaim` goes `Pending` and stays there, because nothing in the cluster provisions volumes yet. Meanwhile every other package you declared sits in `Pending` with reason `WaitingForZarfState`, because the init-first gate holds non-init packages until the cluster has Zarf state. The cluster looks busy and is stuck.

**Check whether you actually have the cycle before solving it.** It assumes two things: that the registry is backed by a `PersistentVolumeClaim`, and that your storage provider needs storage to start. Two designs break it without any sequencing:

- **Storage that consumes raw devices.** Rook-Ceph with `useAllNodes: true` / `useAllDevices: true` needs no `StorageClass` to come up - it is the thing that provides one.
- **A registry that is not volume-backed.** Point the registry at object storage - a Ceph `ObjectBucketClaim`, or any S3 the cluster can reach - and it never wants a PVC.

With both, what is left is an ordering problem rather than a cycle, and ordering between packages is what `dependsOn` is for. You still cannot order it entirely inside the init package, for a reason that is measured rather than argued: see [Why the init package cannot be split](#why-the-init-package-cannot-be-split).

You break the cycle in one of five ways. They are listed cheapest-first, and the first one is the right answer far more often than it looks.

## Option 1: give the cluster a registry it does not have to build
A registry the cluster does not have to build is the end of the whole problem: `zarf init` needs no storage, there is no seed-to-permanent handover to sequence, and your storage driver becomes an ordinary day-two package with no special position in the order.

```sh
zarf init --registry-url=registry.internal --registry-push-username=... --registry-push-password=...
```

Through this provider, that is the init package's own values rather than a flag, and the registry address Zarf records in state is what every package's image references are rewritten to - so a package built to pull from `{{ .State.Registry.Address }}` works unchanged in this mode.

Two things it needs, and they are both about the workload cluster rather than the management cluster. **Every node must be able to reach that registry**, because the node is what pulls; and Zarf writes a pull secret into each namespace it deploys into, which is where those credentials come from. If the registry is behind credentials or a private CA, the controller side of that is `pullSecretRef`, `caBundleConfigMapRef` and friends on the package source - see [Getting started](getting-started.md#reach-a-registry-that-needs-credentials-or-a-private-ca).

This is the right answer whenever a registry the workload cluster can reach already exists. Do not assume one does: an air gap is a topology, not a single pattern, and a management cluster holding the packages does not imply the workload clusters may pull from it - the segment may be routed, firewalled, or physically separate. Where that is the case this option is simply unavailable, and options 2 through 5 are the whole menu.

## Option 2: provide storage outside the package system

If a `StorageClass` already exists when `zarf init` runs, there is no cycle and no ordering problem. Nothing to sequence, nothing to stage, one proxy.

This is the right answer whenever the storage is something your cluster images or your infrastructure provider can carry: `local-path` on a single-node or lab cluster, a CSI driver your node image already ships, a cloud provider's default class, or a Cluster API `ClusterResourceSet` that applies the provisioner as the cluster comes up.

`local-path` as a `ClusterResourceSet`, applied by Cluster API itself at cluster creation:

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: local-path-storage
  namespace: default
data:
  # The provisioner's own manifest. Stage it from
  # https://github.com/rancher/local-path-provisioner/blob/v0.0.37/deploy/local-path-storage.yaml
  local-path-storage.yaml: |
    # ... contents of local-path-storage.yaml ...
---
apiVersion: addons.cluster.x-k8s.io/v1beta1
kind: ClusterResourceSet
metadata:
  name: local-path-storage
  namespace: default
spec:
  clusterSelector:
    matchLabels:
      storage: local-path
  resources:
    - name: local-path-storage
      kind: ConfigMap
  strategy: ApplyOnce
```

Then the whole Zarf side is one proxy, because by the time it runs there is storage:

```yaml
apiVersion: addons.colonel-byte.dev/v1alpha1
kind: ZarfPackageProxy
metadata:
  name: baseline
  namespace: default
spec:
  clusterSelector:
    matchLabels:
      zarf: enabled
  package:
    repository: registry.internal/packages/baseline
    version: "1.0.0"
    api: v1beta1
  init:
    autoInit: true
    package:
      repository: registry.internal/packages/init-slim
      version: "v1.0.1"
      api: v1beta1
    components:
      - name: zarf-injector
      - name: zarf-seed-registry
      - name: zarf-registry
      - name: zarf-agent
```

Two details in that `init` block that are easy to get wrong:

- **`init.components` is not optional in practice.** An init package built to bring a cluster up in stages marks every component optional, so the default selection - required components only - is empty, and the init deploys nothing while reporting success. Name the components you want.
- **`autoInit` is keyed on observed cluster state, never on a name.** The controller probes for Zarf state and only initializes a cluster that has none, so this commutes with a fleet that manages init through its own proxy. Whoever gets there first wins, and the other no-ops.

**Two things `local-path` will want on a real cluster**, both learned by running it rather than reading about it. Its volume-provisioning helper pod mounts a `hostPath`, which the `baseline` Pod Security standard forbids - so its namespace needs `pod-security.kubernetes.io/enforce: privileged`. And a single-node cluster whose only node carries a control-plane `NoSchedule` taint has nowhere to put the registry: Zarf's injector treats any `NoSchedule` or `NoExecute` taint as disqualifying and reports `no suitable injector image or node exists`.

## Option 3: let the whole thing wait on a seeded registry

If your storage provider can run from an ephemeral registry, you can order the pieces with `dependsOn` and let the controller sequence them. The seed registry that `zarf init` stands up before the permanent one is enough to pull from:

```yaml
apiVersion: addons.colonel-byte.dev/v1alpha1
kind: ZarfPackageProxy
metadata:
  name: zarf-init
  namespace: default
spec:
  clusterSelector:
    matchLabels:
      zarf: enabled
  strategy: Reconcile
  deletionPolicy: Retain
  package:
    repository: registry.internal/packages/init-slim
    version: "v1.0.1"
    api: v1beta1
  components:
    - name: zarf-injector
    - name: zarf-seed-registry
    - name: zarf-registry
    - name: zarf-agent
---
apiVersion: addons.colonel-byte.dev/v1alpha1
kind: ZarfPackageProxy
metadata:
  name: storage
  namespace: default
spec:
  clusterSelector:
    matchLabels:
      zarf: enabled
  dependsOn:
    - zarf-init
  package:
    repository: registry.internal/packages/local-path
    version: "0.0.37"
    api: v1beta1
```

The init package is declared as a package in its own right here, not through `init.autoInit`. It resolves as `ZarfInitConfig`, which makes it init-capable, so the gate lets it take the cluster's deploy slot while everything else waits. `dependsOn` then holds `storage` until that release reports deployed, and `Retain` keeps a later deletion of the proxy from uninstalling the thing the cluster is built on.

Whether this works depends on your storage provider tolerating a registry that is about to be replaced. If it does not, you are in option 2 or option 5.

## Option 4: stage it in two passes, like the Ansible playbook

The Ansible flow deploys the init package **twice**: once with seed components only, then the storage package, then init again with the full component set - because the permanent registry cannot come up until storage exists, and storage cannot be pulled until the seed registry does.

You can do the same here, with one proxy and two commits. Declare it with the seed components:

```yaml
spec:
  components:
    - name: zarf-injector
    - name: zarf-seed-registry
```

let the storage proxy land, then edit the same proxy to the full set:

```yaml
spec:
  components:
    - name: zarf-injector
    - name: zarf-seed-registry
    - name: zarf-registry
    - name: zarf-agent
```

Editing the component list re-renders the deploy and the package redeploys with the wider selection, which is Zarf doing an in-place upgrade of a package it already has a record for.

**What you cannot do is split those two passes across two packages, one per proxy.** The reason is not this provider's missing `takeOwnership` knob, which is what it looks like; it is that the seed registry and the permanent registry are *the same Helm release*, so two packages that each carry one of them fight over it - measured in [Why the init package cannot be split](#why-the-init-package-cannot-be-split). So the staging is a sequence of edits - a GitOps commit each - rather than something the DAG performs for you. If your bootstrap genuinely needs two passes with nobody in between, option 1 removes the need and option 5 is the one that performs it.

## Option 5: put the storage inside a custom init package

Zarf's component `import` mechanism composes a `ZarfInitConfig` that carries your storage provider's components alongside the registry's. One package, one proxy, no ordering - the cycle is resolved inside the package instead of between packages.

This is the documented escape hatch for exactly this constraint, and it is the only option that performs the whole bootstrap with nobody in between. It is listed last because its cost is higher than it looks, and part of that cost is measurable.

Each fleet segment needing different storage carries its own init package - that is the obvious half. The half worth knowing is that **you then cannot also manage that storage driver with a proxy of its own on the same cluster.** The bundled init and the standalone driver package would install the same Helm releases, which is the shared-release collision below: whichever ran last owns the resources, removing either uninstalls them, and re-running the other reverts their values. So bundling genuinely forfeits independent storage updates rather than merely being untidy.

Worth it when storage cannot exist before init and no reachable registry exists. Otherwise prefer options 1 through 4.

## Why the init package cannot be split

It is tempting to cut the init package in two - `zarf-injector` + `zarf-seed-registry` in one package, `zarf-registry` + `zarf-agent` in another - and let `dependsOn` sequence storage between them. It looks like the clean answer: three packages, three proxies, no commit in the middle, and each piece versioned on its own.

It does not work, and the way it fails is quiet. Two `ZarfInitConfig`s composed from one upstream set, deployed in order onto a bare cluster:

| Step                                    | What happens                                                                                                                  |
| --------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------- |
| Deploy the seed half                    | Succeeds. Helm release `zarf-docker-registry` at revision 1, its objects labelled `zarf.dev/package: <seed package>`          |
| Deploy the registry half                | Succeeds, with no warning - Zarf logs `performing Helm upgrade` and exits 0                                                   |
| …but the ownership label                | silently flips to `zarf.dev/package: <registry package>`, and **both** deployed-package records now claim that chart          |
| Remove the seed half                    | **Destroys the live registry.** The deployment and the release secrets go, and the other record is left claiming a dead chart |
| Re-run the seed half, as a version bump | **Reverts the shared release.** `helm get values` shows the permanent settings replaced by the seed's                         |

The cause is that `zarf-seed-registry` and `zarf-registry` are not two deployments of two things. They are one Helm release lineage - the seed is revision 1 of `zarf-docker-registry` and the permanent registry is revision 2. Zarf never objects, because the release name matches and it simply re-labels what it finds. Two packages each carrying one revision therefore both believe they own the registry, and the last writer wins at every event.

The last row is the one that matters, because independent versioning is the whole reason to split: bumping the seed half downgrades the permanent registry to seed configuration. With upstream's `seedRegistry.persistence.enabled: false`, that revert strips the registry's storage configuration, not merely its autoscaling.

**The general rule, which reaches further than init:** two Zarf packages that install the same Helm release cannot both be managed on one cluster. Nothing refuses the second deploy, so the collision shows up later - as a removal that takes a live component with it, or a redeploy that reverts another package's configuration. When you split a package, cut between Helm releases, never through one. `zarf-agent` and `git-server` are safely separable for exactly this reason: separate charts, separate releases, no shared lineage.

If you do end up with two packages over one release, `deletionPolicy: Retain` on both is not optional - the default `Delete` runs precisely the removal that destroyed the registry above.

## Day two

Once a cluster has Zarf state, none of the above applies to it again. The gate is open, `autoInit` no-ops because it probes and finds state, and packages deploy in whatever order `dependsOn` describes.

The Ansible playbook distinguishes bootstrap from convergence by inspecting what is installed and branching. Here that distinction does not exist, because the controller re-derives it on every pass: a release that is already on the declared digest is left alone, a release whose proxy moved to a new version redeploys, and `ApplyOnce` packages adopt zarf's existing record rather than re-running. You declare the end state once and the same declaration is correct on day one and day two.

What still needs a person:

- **A new version** - bump `spec.package.version`. Nothing moves on its own, including a re-pushed tag, which raises `TagDrift` and waits.
- **A failed deploy** - the controller does not yet retry on a timer. Editing the spec is what re-runs it.
- **A failed init** - the release holds with `InitFailed` and the failing Job is kept as evidence. Nothing deploys onto a half-initialized cluster, which is deliberate: a cluster with Zarf state and no registry is indistinguishable from a properly initialized one from the outside, so the provider refuses to guess.

## Migrating a management cluster

`clusterctl move` carries a `Cluster` to a new management cluster. It does not carry your `ZarfPackageProxy` objects, and that is not a defect: a proxy is owned by nothing on purpose - it selects clusters by label and spans a fleet - so it is in no cluster's object graph and clusterctl has no reason to associate it with the cluster being moved. The `ZarfPackageRelease` objects do not survive either; they are owner-referenced to the `Cluster`, so Kubernetes garbage-collects them when the move deletes the source `Cluster` at the end.

What is left untouched is the part that matters. **Zarf's record of what is deployed lives on the workload cluster, not on the management cluster**, so a migration loses bookkeeping, not state. Verified by moving one: the deployed content and `zarf-package-<name>` secrets are exactly as they were.

So the runbook is:

1. `clusterctl move --to-kubeconfig=...` as normal. The clusters arrive.
2. **Re-apply your proxies to the new management cluster.** They are declarations and should already be in Git, which is the whole reason this step is cheap.
3. The controller re-stamps a release per matching cluster and converges.

Step 3 behaves differently by strategy, and it is worth knowing which you have:

| `spec.strategy` | What happens after the proxies are re-applied                                                                                               |
| --------------- | ------------------------------------------------------------------------------------------------------------------------------------------- |
| `ApplyOnce`     | The rebuilt release probes the cluster, finds Zarf's existing record, and completes with reason `AdoptedExistingRecord`. Nothing re-runs    |
| `Reconcile`     | The rebuilt release has no deploy history to compare against, so it deploys the declared digest again - a redeploy per cluster, not a no-op |

That `Reconcile` row is the one to plan around. The redeploy is of the same digest, so it converges on the state the cluster is already in, but on a large fleet it is a burst of deploy Jobs and registry pulls immediately after a migration. One deploy Job runs per cluster at a time, so it is paced rather than simultaneous - but pace it further if that matters: move in batches, or re-apply the proxies in batches rather than all at once.

## Checking where a cluster is

```sh
kubectl get zarfpackagerelease -o custom-columns='NAME:.metadata.name,CLUSTER:.spec.clusterRef.name,PHASE:.status.phase,READY:.status.conditions[?(@.type=="Ready")].reason'
```

A fleet mid-bootstrap reads as a column of `Pending`/`WaitingForZarfState` with one `Deploying` - the init package holding its cluster's slot. That is the system working; one deploy or removal Job runs per cluster at a time, so the ordering you declared is an invariant rather than a race.

To see whether a cluster is initialized at all, ask the cluster:

```sh
kubectl --kubeconfig ./workload.kubeconfig get secret -n zarf zarf-state
```

And what Zarf believes is deployed there:

```sh
kubectl --kubeconfig ./workload.kubeconfig get secret -n zarf -o name | grep zarf-package-
```

Those secrets, not the objects on the management cluster, are what `ApplyOnce` adoption and uninstall both key on. Deleting a release does not delete them; that is why a recreated release adopts instead of re-running a seed.
